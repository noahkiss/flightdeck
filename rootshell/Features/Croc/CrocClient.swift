#if !targetEnvironment(macCatalyst)

import CryptoKit
import Darwin
import Foundation
import OSLog

/// Core croc transfer client — orchestrates the full send/receive protocol.
/// Port of Go's `croc.Client` struct and `transfer()` method.
///
/// Deliberately NOT `@MainActor` — all heavy work (network I/O, file hashing,
/// PAKE crypto, compression) runs on a cooperative-thread-pool executor so the
/// UI stays responsive.  Output closures hop to whatever context the caller
/// needs (typically MainActor) on their own.
nonisolated final class CrocClient: @unchecked Sendable {

    private nonisolated static let logger = Logger(subsystem: "com.rootshell.croc", category: "CrocClient")

    // MARK: - Configuration

    var options: CrocOptions
    let output: @Sendable (String) -> Void
    let outputData: @Sendable (Data) -> Void

    // MARK: - PAKE & Crypto State

    private var pake: CrocPAKE?
    private var sessionKey: Data?

    // MARK: - Transfer State

    private(set) var phase: CrocTransferPhase = .idle
    private var connections: [CrocComm] = []
    private var localRelays: [CrocRelayServer] = []
    private var broadcastTask: Task<Void, Never>?
    private var cancelled = false
    private var startTime: Date?
    private var usingLocalRelay = false
    private var localRelayPorts: [String] = []
    private var externalIP = ""
    private var externalIPConnected = ""

    // MARK: - File State

    private var filesToTransfer: [CrocFileInfo] = []
    private var emptyFoldersToTransfer: [CrocFileInfo] = []
    private var totalNumberFolders = 0
    private var totalSent: Int64 = 0

    // MARK: - Receiver Data Tasks (long-lived, one per data port)

    /// Shared state for long-lived receiver data tasks.
    /// Go starts receiveData(j) goroutines once and reuses them across all files.
    /// Protected by `receiverLock` — mirrors Go's `c.mutex`.
    private var currentFile: FileHandle?
    private var currentFileIsClosed = true
    private var currentFileChunks: [Int64] = []
    private var totalChunksTransferred = 0
    private var receiverDataTasks: [Task<Void, Never>] = []
    /// Set by a data task that hit a fatal error; `executeReceive` rethrows it. Guarded by `receiverLock`.
    private var receiverDataError: Error?
    private let receiverLock = NSLock()
    private var receiverProgressBar: CrocProgress?

    private struct PreparedRelayConnection: Sendable {
        let comm: CrocComm
        let relayAddress: String
        let relayPorts: [String]
        let usingLocalRelay: Bool
        let externalIP: String
    }

    // MARK: - Callbacks

    /// Called when user input is needed (accept files, resume, etc.)
    var onPrompt: (@Sendable (String, @escaping @Sendable (String) -> Void) -> Void)?

    /// The continuation for a pending prompt, if any.  Stored so that
    /// `cancel()` can resume it with a rejection to unblock the task.
    private var pendingPromptContinuation: CheckedContinuation<Bool, Never>?

    init(options: CrocOptions, output: @escaping @Sendable (String) -> Void, outputData: @escaping @Sendable (Data) -> Void) {
        self.options = options
        self.output = output
        self.outputData = outputData
    }

    private typealias S = TerminalStyle

    private func writeStatus(_ text: String) {
        guard !options.quiet else { return }
        output(text)
    }

    private func writeDebug(_ text: String) {
        guard options.debug, !options.quiet else { return }
        output(S.fg(S.dim, "[debug] \(text)") + "\r\n")
    }

    private func promptYesNo(_ prompt: String, defaultYes: Bool = true) async -> Bool {
        guard let onPrompt else { return defaultYes }
        return await withCheckedContinuation { (continuation: CheckedContinuation<Bool, Never>) in
            self.pendingPromptContinuation = continuation
            onPrompt(prompt) { [weak self] response in
                // Guard against double-resume (cancel() may have already resumed).
                guard let self, self.pendingPromptContinuation != nil else { return }
                self.pendingPromptContinuation = nil
                let answer = response.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
                if answer.isEmpty {
                    continuation.resume(returning: defaultYes)
                } else {
                    continuation.resume(returning: answer == "y" || answer == "yes")
                }
            }
        }
    }

    private func cleanupTemporaryTransferFiles() {
        for file in filesToTransfer where file.tempFile {
            let fullPath = file.folderSource.isEmpty
                ? file.name
                : (file.folderSource as NSString).appendingPathComponent(file.name)
            try? FileManager.default.removeItem(atPath: fullPath)

            let parentDir = (fullPath as NSString).deletingLastPathComponent
            if parentDir.contains("croc_zip_") {
                try? FileManager.default.removeItem(atPath: parentDir)
            }
        }
    }

    // MARK: - Public API

    /// Send files.
    func send(paths: [String]) async throws {
        #if FLIGHTDECK
        throw CrocError.protocolError("croc is disabled in flightdeck")
        #endif
        startTime = Date()
        options.isSender = true
        usingLocalRelay = options.onlyLocal

        // Ensure local relay resources are cleaned up on all exit paths
        // (success, error, or cancellation).
        defer {
            broadcastTask?.cancel()
            broadcastTask = nil
            let relays = localRelays
            localRelays.removeAll()
            Task { for relay in relays { await relay.cancel() } }
        }

        // Collect file info
        let (files, emptyFolders, numFolders) = try CrocSender.collectFilesInfo(
            paths: paths,
            hashAlgorithm: options.hashAlgorithm,
            zipFolder: options.zipFolder,
            gitIgnore: options.gitIgnore,
            exclude: options.exclude
        )
        filesToTransfer = files
        emptyFoldersToTransfer = emptyFolders
        totalNumberFolders = numFolders

        guard !filesToTransfer.isEmpty || !emptyFoldersToTransfer.isEmpty else {
            throw CrocError.fileNotFound("no files to send")
        }

        defer { cleanupTemporaryTransferFiles() }

        // Generate code phrase if not provided
        if options.sharedSecret.isEmpty {
            options.sharedSecret = CrocMnemonicode.getRandomName()
        }
        let code = options.sharedSecret

        // Display info
        let totalSize = filesToTransfer.reduce(0) { $0 + $1.size }
        let countStr = filesToTransfer.count == 1 ? "1 file" : "\(filesToTransfer.count) files"
        writeStatus(S.fg(S.info, "🐊 Sending \(countStr)") + S.fg(S.dim, " (\(CrocUtils.byteCountDecimal(totalSize)))") + "\r\n")
        writeStatus("🐊 Code is: " + S.boldFg(S.success, code) + "\r\n")

        let receiveFlags = receiveCommandFlags()
        writeStatus(S.fg(S.dim, "On the other computer run:") + "\r\n")
        writeStatus(S.fg(S.dim, "(For Windows)") + "\r\n")
        writeStatus("    " + S.fg(S.cyan, "croc \(receiveFlags)\(code)") + "\r\n")
        writeStatus(S.fg(S.dim, "(For Linux/macOS)") + "\r\n")
        let trimmedFlags = receiveFlags.trimmingCharacters(in: .whitespaces)
        let linuxCmd = trimmedFlags.isEmpty ? "" : "croc \(trimmedFlags)"
        writeStatus("    " + S.fg(S.cyan, "CROC_SECRET=\"\(code)\" \(linuxCmd.isEmpty ? "croc" : linuxCmd)") + "\r\n")

        if !options.disableClipboard {
            CrocPresentation.copyToClipboard(
                code: code,
                flags: receiveFlags.trimmingCharacters(in: .whitespaces),
                extended: options.extendedClipboard,
                quiet: options.quiet,
                output: output
            )
        }
        if options.showQrCode, let qr = CrocPresentation.renderQRCode(for: code) {
            writeStatus(qr + "\r\n")
        }
        if options.ask {
            writeStatus(S.fg(S.dim, "Your machine ID is '\(CrocPresentation.machineID())'") + "\r\n")
        }

        // Generate room name
        options.roomName = CrocRelayClient.roomName(from: options.sharedSecret)

        if !options.disableLocal {
            try await startLocalRelay()
        }

        // Connect to relay
        try await connectAndTransfer()
    }

    /// Receive files using a code phrase.
    func receive(code: String) async throws {
        #if FLIGHTDECK
        throw CrocError.protocolError("croc is disabled in flightdeck")
        #endif
        startTime = Date()
        options.isSender = false
        options.sharedSecret = code
        options.roomName = CrocRelayClient.roomName(from: code)

        if !options.ip.isEmpty {
            options.disableLocal = true
            if CrocPresentation.looksLikeIPv6Address(options.ip) {
                options.relayAddress6 = options.ip
                options.relayAddress = ""
            } else {
                options.relayAddress = options.ip
                options.relayAddress6 = ""
            }
        }

        // Try LAN discovery first
        if !options.disableLocal {
            if let peer = await CrocPeerDiscovery.discover(multicastAddress: options.multicastAddress) {
                var port = String(peer.payload.dropFirst(4)) // Remove "croc" prefix
                if port.isEmpty { port = CrocConstants.defaultPort }
                // Bracket IPv6 hosts (Go's net.JoinHostPort)
                let candidate = peer.address.contains(":")
                    ? "[\(peer.address)]:\(port)"
                    : "\(peer.address):\(port)"
                // Verify the peer's local relay is reachable before committing,
                // matching Go's tcp.PingServer gate; otherwise fall back to the
                // public relay.
                if await CrocRelayClient.ping(address: candidate, options: options) {
                    writeStatus(S.fg(S.success, "🐊 Found peer on LAN: \(peer.address)") + "\r\n")
                    options.relayAddress = candidate
                    usingLocalRelay = true
                }
            }
        }

        if options.onlyLocal && options.relayAddress.isEmpty {
            throw CrocError.connectionFailed("found no local addresses to connect")
        }

        try await connectAndTransfer()
    }

    /// Start a standalone relay server.
    func startRelay() async throws {
        #if FLIGHTDECK
        throw CrocError.protocolError("croc is disabled in flightdeck")
        #endif
        let relayPorts = findOpenPorts(starting: options.port, count: max(options.transfers, 2))
        guard relayPorts.count >= max(options.transfers, 2) else {
            throw CrocError.connectionFailed("not enough open ports to run relay")
        }

        let banner = relayPorts.dropFirst().map(String.init).joined(separator: ",")

        let appVersion = Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "unknown"
        let buildNumber = Bundle.main.infoDictionary?["CFBundleVersion"] as? String ?? "unknown"

        func ts() -> String {
            let now = Date()
            let cal = Calendar.current
            let c = cal.dateComponents([.year, .month, .day, .hour, .minute, .second], from: now)
            return String(format: "%04d/%02d/%02d %02d:%02d:%02d",
                          c.year!, c.month!, c.day!, c.hour!, c.minute!, c.second!)
        }

        func relayLog(_ msg: String) {
            writeStatus(S.fg(S.dim, "\(ts()) ") + msg + "\r\n")
        }

        relayLog(S.fg(S.info, "🐊 Starting rootshell croc relay") + S.fg(S.dim, " \(appVersion) (\(buildNumber)) (compatible with croc \(CrocConstants.version))"))

        for port in relayPorts {
            let relay = CrocRelayServer(
                host: "0.0.0.0",
                port: UInt16(port),
                password: options.relayPassword,
                banner: banner
            )
            localRelays.append(relay)
            try await relay.start()
            relayLog(S.fg(S.dim, "Listening on ") + S.fg(S.info, ":\(port)"))
        }

        relayLog(S.fg(S.success, S.checkIcon + " Relay is running") + S.fg(S.dim, " (Ctrl-C to stop)"))

        while !cancelled && !Task.isCancelled {
            try? await Task.sleep(for: .seconds(1))
        }

        // Clean shutdown — stop all relay listeners and close rooms.
        for relay in localRelays {
            await relay.cancel()
        }
        relayLog(S.fg(S.dim, "Relay stopped."))
    }

    /// Cancel the transfer.
    func cancel() {
        cancelled = true

        // Resume any pending prompt so the task unblocks immediately.
        if let cont = pendingPromptContinuation {
            pendingPromptContinuation = nil
            cont.resume(returning: false)
        }

        broadcastTask?.cancel()
        for conn in connections {
            conn.close()
        }
        for task in receiverDataTasks { task.cancel() }
        let relays = localRelays
        localRelays.removeAll()
        Task { for relay in relays { await relay.cancel() } }
        phase = .cancelled
    }

    // MARK: - Core Transfer Logic

    /// Any error tears down connections and receiver tasks so the peer
    /// doesn't sit waiting on a message we'll never send.
    private func connectAndTransfer() async throws {
        do {
            try await runTransfer()
        } catch {
            abortTransfer()
            throw error
        }
    }

    private func abortTransfer() {
        for conn in connections {
            conn.close()
        }
        for task in receiverDataTasks { task.cancel() }
        receiverLock.withLock {
            try? currentFile?.close()
            currentFile = nil
            currentFileIsClosed = true
        }
    }

    private func runTransfer() async throws {
        connections.removeAll()
        receiverDataTasks.removeAll()
        receiverDataError = nil
        try throwIfCancelled()

        if options.isSender {
            phase = .waitingForPeer
            let prepared = try await prepareSenderControlConnection()
            if cancelled || Task.isCancelled {
                prepared.comm.close()
                throw CrocError.cancelled
            }

            usingLocalRelay = prepared.usingLocalRelay
            externalIP = prepared.externalIP
            options.relayAddress = prepared.relayAddress
            options.relayPorts = prepared.relayPorts
            connections.append(prepared.comm)
        } else {
            // Resolve initial relay address
            if options.relayAddress.isEmpty && options.relayAddress6.isEmpty {
                if options.onlyLocal {
                    throw CrocError.connectionFailed("found no local relay address")
                }
                options.relayAddress = "\(CrocConstants.defaultRelayHost):\(CrocConstants.defaultPort)"
                options.relayAddress6 = "\(CrocConstants.defaultRelay6Host):\(CrocConstants.defaultPort)"
            }

            let relay = try await connectToRelay(room: options.roomName)
            if cancelled || Task.isCancelled {
                relay.comm.close()
                throw CrocError.cancelled
            }
            externalIP = relay.remoteIP
            writeDebug("Connected to relay \(options.relayAddress)")
            connections.append(relay.comm)

            let relayPorts = parseRelayPorts(relay.banner)
            if options.noMultiplexing, let firstPort = relayPorts.first {
                options.relayPorts = [firstPort]
            } else {
                options.relayPorts = relayPorts
            }

            phase = .waitingForPeer

            guard let controlConn = connections.first else {
                throw CrocError.channelNotSecured
            }

            // Pre-transfer handshake: SimpleMessage-based PAKE + IP exchange.
            // Receiver may still replace the control connection during pre-transfer,
            // so subsequent work must use the updated relay address and ports.
            try throwIfCancelled()
            try await receiverPreTransfer(controlConn: controlConn)
        }

        phase = .securingChannel

        // Transfer-level PAKE (CrocMessage-wrapped) — this is the REAL key exchange.
        // Go opens the multiplexed data channels only after this succeeds.
        guard let controlConn = connections.first else {
            throw CrocError.channelNotSecured
        }
        try await performTransferPAKE(controlConn: controlConn)

        guard let key = sessionKey else {
            throw CrocError.channelNotSecured
        }
        try throwIfCancelled()

        // For receiver: start long-lived receiveData goroutines for each data port.
        // These run for the entire transfer session, not per-file.
        if !options.isSender {
            startReceiverDataTasks(key: key)
        }

        // Execute transfer
        if options.isSender {
            try await executeSend(key: key)
        } else {
            try await executeReceive(key: key)
        }
    }

    // MARK: - Pre-Transfer Handshake (SimpleMessage-based)

    /// Sender waits for receiver's SimpleMessage PAKE, responds, then waits for handshake.
    /// Matches Go's sender loop at croc.go:764-853.
    private func senderPreTransfer(controlConn: CrocComm) async throws {
        phase = .pakeExchange

        if usingLocalRelay {
            try await waitForLocalHandshake(controlConn: controlConn)
            return
        }

        try await senderPublicPreTransfer(controlConn: controlConn)
    }

    private func senderPublicPreTransfer(controlConn: CrocComm) async throws {
        try await Self.senderPublicPreTransfer(
            controlConn: controlConn,
            sharedSecret: options.sharedSecret,
            curve: options.curve,
            disableLocal: options.disableLocal,
            localRelayPorts: localRelayPorts,
            cancelled: { [weak self] in self?.cancelled ?? true }
        )
    }

    private static func senderPublicPreTransfer(
        controlConn: CrocComm,
        sharedSecret: String,
        curve: String,
        disableLocal: Bool,
        localRelayPorts: [String],
        cancelled: @escaping @Sendable () -> Bool
    ) async throws {
        if Task.isCancelled || cancelled() {
            throw CrocError.cancelled
        }

        let pw = Data(Array(sharedSecret.utf8).dropFirst(min(5, sharedSecret.count)))
        let pakeB = try CrocPAKE(pw: pw, role: 1, curve: curve)

        var kB: Data?  // Session key from SimpleMessage PAKE

        // Message loop: receive SimpleMessage JSON from relay pipe
        while !Task.isCancelled && !cancelled() {
            let data = try await controlConn.receiveSkippingProbes()

            // Try to decode as SimpleMessage
            if let simpleMsg = try? JSONDecoder().decode(CrocSimpleMessage.self, from: data) {
                if simpleMsg.kind == "pake1", let pakeBytes = simpleMsg.bytes {
                    // Receiver's PAKE init
                    try pakeB.update(pakeBytes)
                    guard let sk = pakeB.sessionKey else {
                        throw CrocError.channelNotSecured
                    }
                    kB = sk

                    // Send pake2 response
                    let response = CrocSimpleMessage(bytes: try pakeB.bytes(), kind: "pake2")
                    let responseData = try JSONEncoder().encode(response)
                    try await controlConn.send(responseData)
                    continue
                }
            }

            // Check for encrypted ips? request (after kB is derived).
            // Go aborts on GCM auth failure (relay sent message encrypted with invalid key).
            if let kB {
                do {
                    let decrypted = try CrocEncryption.decrypt(data, key: kB)
                    if decrypted == Data("ips?".utf8) {
                        // Send local IPs encrypted with kB
                        var ips: [String] = []
                        if !disableLocal, let localControlPort = localRelayPorts.first {
                            ips = CrocFileUtils.getLocalIPs()
                            ips.insert(localControlPort, at: 0)
                        }
                        let ipsData = try JSONEncoder().encode(ips)
                        let encrypted = try CrocEncryption.encrypt(ipsData, key: kB)
                        try await controlConn.send(encrypted)
                        continue
                    }
                } catch {
                    // GCM auth failure = potential security issue; abort like Go does.
                    // Non-encrypted messages (SimpleMessage, handshake) fall through below.
                    let desc = error.localizedDescription
                    if desc.contains("authentication") || desc.contains("seal") || desc.contains("tag") {
                        throw CrocError.decryptionFailed
                    }
                    // Not encrypted data — fall through to handshake check
                }
            }

            // Check for handshake signal
            if data == Data("handshake".utf8) {
                Self.logger.debug("Sender pre-transfer handshake complete")
                return
            }
        }

        throw CrocError.cancelled
    }

    private func waitForLocalHandshake(controlConn: CrocComm) async throws {
        try await Self.waitForLocalHandshake(
            controlConn: controlConn,
            cancelled: { [weak self] in self?.cancelled ?? true }
        )
    }

    private static func waitForLocalHandshake(
        controlConn: CrocComm,
        cancelled: @escaping @Sendable () -> Bool
    ) async throws {
        while !Task.isCancelled && !cancelled() {
            let data = try await controlConn.receiveSkippingProbes()
            if data == Data("handshake".utf8) {
                Self.logger.debug("Sender local pre-transfer handshake complete")
                return
            }
        }

        throw CrocError.cancelled
    }

    /// Receiver sends SimpleMessage PAKE, requests IPs, then sends handshake.
    /// Matches Go's receiver flow at croc.go:1030-1151.
    private func receiverPreTransfer(controlConn: CrocComm) async throws {
        phase = .pakeExchange

        if usingLocalRelay {
            try await controlConn.send(Data("handshake".utf8))
            Self.logger.debug("Receiver local pre-transfer handshake complete")
            return
        }

        let pw = Data(Array(options.sharedSecret.utf8).dropFirst(min(5, options.sharedSecret.count)))
        let pakeA = try CrocPAKE(pw: pw, role: 0, curve: options.curve)

        // Send pake1
        let pake1 = CrocSimpleMessage(bytes: try pakeA.bytes(), kind: "pake1")
        let pake1Data = try JSONEncoder().encode(pake1)
        try await controlConn.send(pake1Data)

        // Receive pake2
        let pake2Data = try await controlConn.receiveSkippingProbes()
        let pake2 = try JSONDecoder().decode(CrocSimpleMessage.self, from: pake2Data)
        guard pake2.kind == "pake2", let pake2Bytes = pake2.bytes else {
            throw CrocError.pakeExchangeFailed("expected pake2 response")
        }
        try pakeA.update(pake2Bytes)
        guard let kA = pakeA.sessionKey else {
            throw CrocError.channelNotSecured
        }

        // Request sender's local IPs (encrypted with kA)
        if !options.disableLocal {
            let ipsRequest = try CrocEncryption.encrypt(Data("ips?".utf8), key: kA)
            try await controlConn.send(ipsRequest)

            let ipsResponseData = try await controlConn.receiveSkippingProbes()
            if let decrypted = try? CrocEncryption.decrypt(ipsResponseData, key: kA),
               let ips = try? JSONDecoder().decode([String].self, from: decrypted),
               ips.count > 1 {
                // ips[0] = port, ips[1..] = IP addresses
                let port = ips[0]
                for ip in ips.dropFirst() {
                    let serverTry = "\(ip):\(port)"
                     if let localConn = try? await CrocRelayClient.connect(
                         to: serverTry,
                          password: options.relayPassword,
                          room: options.roomName,
                          options: options,
                          timeout: CrocConstants.localRelayConnectTimeout
                      ) {
                        Self.logger.info("Switched to local relay: \(serverTry)")
                        // Replace control connection with local one
                        connections[0].close()
                        connections[0] = localConn.comm
                        options.relayAddress = serverTry
                        let localPorts = parseRelayPorts(localConn.banner)
                        if options.noMultiplexing, let firstPort = localPorts.first {
                            options.relayPorts = [firstPort]
                        } else {
                            options.relayPorts = localPorts
                        }
                        usingLocalRelay = true
                        externalIP = localConn.remoteIP
                        externalIPConnected = serverTry
                        break
                    }
                }
            }
        }

        // Send handshake signal
        let handshakeConn = connections.first!
        try await handshakeConn.send(Data("handshake".utf8))

        Self.logger.debug("Receiver pre-transfer handshake complete")
    }

    // MARK: - Transfer-Level PAKE (CrocMessage-wrapped)

    /// Perform the transfer-level PAKE exchange using CrocMessage.
    /// This is the REAL key exchange that produces the session encryption key.
    /// Called at the start of the transfer() message loop.
    private func performTransferPAKE(controlConn: CrocComm) async throws {
        let pw = Data(Array(options.sharedSecret.utf8).dropFirst(min(5, options.sharedSecret.count)))

        if options.isSender {
            // Sender is role 1 in the transfer-level PAKE
            let data = try await controlConn.receiveSkippingProbes()
            let msg = try CrocMessage.decode(data: data, key: nil)
            guard msg.type == .pake, let pakeBytes = msg.bytes else {
                throw CrocError.pakeExchangeFailed("expected transfer PAKE")
            }

            // Use the receiver's curve choice if provided, matching Go's processMessagePake
            let receiverCurve: String
            if let curveBytes = msg.bytes2, let curve = String(data: curveBytes, encoding: .utf8), !curve.isEmpty {
                receiverCurve = curve
            } else {
                receiverCurve = options.curve
            }

            let pake = try CrocPAKE(pw: pw, role: 1, curve: receiverCurve)
            try pake.update(pakeBytes)
            guard let sk = pake.sessionKey else { throw CrocError.channelNotSecured }
            var salt = Data(count: 8)
            salt.withUnsafeMutableBytes { _ = SecRandomCopyBytes(kSecRandomDefault, 8, $0.baseAddress!) }
            let (key, _) = try CrocKeyDerivation.deriveKey(passphrase: sk, salt: salt)
            sessionKey = key
            let resp = CrocMessage(type: .pake, bytes: try pake.bytes(), bytes2: salt)
            try await controlConn.send(try resp.encode(key: nil))

        } else {
            // Receiver is role 0
            let pake = try CrocPAKE(pw: pw, role: 0, curve: options.curve)
            let pakeMsg = CrocMessage(type: .pake, bytes: try pake.bytes(), bytes2: Data(options.curve.utf8))
            try await controlConn.send(try pakeMsg.encode(key: nil))

            let respData = try await controlConn.receiveSkippingProbes()
            let resp = try CrocMessage.decode(data: respData, key: nil)
            guard resp.type == .pake, let respBytes = resp.bytes, let salt = resp.bytes2 else {
                throw CrocError.pakeExchangeFailed("expected transfer PAKE response")
            }
            try pake.update(respBytes)
            guard let sk = pake.sessionKey else { throw CrocError.channelNotSecured }
            let (key, _) = try CrocKeyDerivation.deriveKey(passphrase: sk, salt: salt)
            sessionKey = key

            try await openDataConnections()

            // Send externalIP message
            let externalIPMsg = CrocMessage(type: .externalIP, message: externalIP)
            try await controlConn.send(try externalIPMsg.encode(key: key))
            Self.logger.debug("Transfer-level PAKE complete, channel secured")
            return
        }

        try await openDataConnections()

        Self.logger.debug("Transfer-level PAKE complete, channel secured")
    }

    // MARK: - Send Execution

    private func executeSend(key: Data) async throws {
        guard let controlConn = connections.first else { return }

        // The transfer() message loop handles PAKE, externalIP, then file transfer.
        // Sender waits for receiver's PAKE, responds, then processes externalIP.
        // This is done via performTransferPAKE in connectAndTransfer already.

        // Handle externalIP exchange: sender receives first, then sends
        let extData = try await controlConn.receiveSkippingProbes()
        let extMsg = try CrocMessage.decode(data: extData, key: key)
        if extMsg.type == .externalIP {
            if externalIPConnected.isEmpty {
                externalIPConnected = extMsg.message ?? ""
            }
            // Send our externalIP back
            let ourExtIP = CrocMessage(type: .externalIP, message: externalIP)
            try await controlConn.send(try ourExtIP.encode(key: key))
        }

        phase = .transferringFileInfo

        // Send file info
        let senderInfo = CrocSenderInfo(
            filesToTransfer: filesToTransfer,
            emptyFoldersToTransfer: emptyFoldersToTransfer,
            totalNumberFolders: totalNumberFolders,
            machineID: CrocPresentation.machineID(),
            ask: options.ask,
            sendingText: options.sendingText,
            noCompress: options.noCompress,
            hashAlgorithm: options.hashAlgorithm
        )

        let infoData = try JSONEncoder().encode(senderInfo)
        let infoMsg = CrocMessage(type: .fileInfo, bytes: infoData)
        try await controlConn.send(try infoMsg.encode(key: key))

        phase = .waitingForAcceptance

        while !cancelled {
            let responseData = try await controlConn.receiveSkippingProbes()
            let response = try CrocMessage.decode(data: responseData, key: key)

            switch response.type {
            case .error:
                throw CrocError.peerError(response.message ?? "unknown error")
            case .finished:
                let finishedAck = CrocMessage(type: .finished)
                try await controlConn.send(try finishedAck.encode(key: key))

                let duration = Date().timeIntervalSince(startTime ?? Date())
                phase = .completed(totalBytes: totalSent, duration: duration)
                writeStatus(transferCompleteMessage(totalBytes: totalSent, duration: duration))
                return
            case .recipientReady:
                guard let requestData = response.bytes else {
                    throw CrocError.protocolError("missing file request data")
                }
                let request = try JSONDecoder().decode(CrocRemoteFileRequest.self, from: requestData)
                guard filesToTransfer.indices.contains(request.filesToTransferCurrentNum) else {
                    throw CrocError.protocolError("invalid requested file index: \(request.filesToTransferCurrentNum)")
                }
                if options.ask {
                    let machineID = request.machineID.isEmpty ? "unknown" : request.machineID
                    let accepted = await promptYesNo("Send to machine '\(machineID)'? (Y/n) ")
                    if !accepted {
                        let errorMsg = CrocMessage(type: .error, message: "refusing files")
                        try await controlConn.send(try errorMsg.encode(key: key))
                        throw CrocError.filesRejected
                    }
                }

                let chunkMap = CrocReceiver.chunkRangesToChunks(request.currentFileChunkRanges)
                let file = filesToTransfer[request.filesToTransferCurrentNum]
                let fileIdx = request.filesToTransferCurrentNum

                phase = .transferringData(fileIndex: fileIdx, totalFiles: filesToTransfer.count, progress: 0)

                let bar = options.sendingText ? nil : CrocProgress(
                    filename: file.name,
                    totalBytes: file.size,
                    fileIndex: fileIdx,
                    fileCount: filesToTransfer.count,
                    output: output
                )

                let dataConns = connections.count > 1 ? Array(connections.dropFirst()) : connections

                try await CrocSender.sendFileData(
                    file: file,
                    connections: dataConns,
                    key: key,
                    chunkMap: chunkMap,
                    noCompress: options.noCompress,
                    throttleBytesPerSecond: CrocPresentation.parseByteRate(options.throttleUpload),
                    onProgress: { [weak self] bytes in
                        self?.totalSent += bytes
                        bar?.update(bytesAdded: bytes)
                    },
                    isCancelled: { [weak self] in
                        self?.cancelled ?? true
                    }
                )

                bar?.finish()

                // Wait for close from receiver
                let closeData = try await controlConn.receiveSkippingProbes()
                let closeMsg = try CrocMessage.decode(data: closeData, key: key)
                if closeMsg.type == .closeSender {
                    // Acknowledge
                    let ack = CrocMessage(type: .closeRecipient)
                    try await controlConn.send(try ack.encode(key: key))
                } else {
                    throw CrocError.unexpectedMessageType(closeMsg.type?.rawValue ?? "nil")
                }

            default:
                throw CrocError.unexpectedMessageType(response.type?.rawValue ?? "nil")
            }
        }

        throw CrocError.transferCancelled
    }

    // MARK: - Receive Execution

    private func executeReceive(key: Data) async throws {
        guard let controlConn = connections.first else { return }

        // Consume the sender's externalIP response before reading file info.
        // The sender sends externalIP (in executeSend) after receiving ours
        // (sent at the end of performTransferPAKE).
        let firstData = try await controlConn.receiveSkippingProbes()
        let firstMsg = try CrocMessage.decode(data: firstData, key: key)
        if firstMsg.type == .externalIP {
            if externalIPConnected.isEmpty {
                externalIPConnected = firstMsg.message ?? ""
            }
            writeDebug("Receiver got sender externalIP: \(firstMsg.message ?? "")")
        }

        phase = .transferringFileInfo

        // Receive file info (or use firstMsg if it was already fileInfo)
        let infoMsg: CrocMessage
        if firstMsg.type == .fileInfo {
            infoMsg = firstMsg
        } else {
            let infoData = try await controlConn.receiveSkippingProbes()
            infoMsg = try CrocMessage.decode(data: infoData, key: key)
        }

        guard infoMsg.type == .fileInfo, let infoBytes = infoMsg.bytes else {
            throw CrocError.unexpectedMessageType(infoMsg.type?.rawValue ?? "nil")
        }

        let senderInfo = try JSONDecoder().decode(CrocSenderInfo.self, from: infoBytes)
        filesToTransfer = senderInfo.filesToTransfer
        emptyFoldersToTransfer = senderInfo.emptyFoldersToTransfer
        if senderInfo.sendingText {
            options.stdout = true
        }
        // Sync sender's compression and hash settings — matches Go's updateIfRecipientHasFileInfo
        options.noCompress = senderInfo.noCompress

        // Validate files
        try CrocReceiver.validateFileInfo(filesToTransfer)

        // Display and prompt for acceptance
        let totalSize = filesToTransfer.reduce(0) { $0 + $1.size }
        let recvCountStr = filesToTransfer.count == 1 ? "1 file" : "\(filesToTransfer.count) files"
        writeStatus(S.fg(S.info, "🐊 Receiving \(recvCountStr)") + S.fg(S.dim, " (\(CrocUtils.byteCountDecimal(totalSize)))") + "\r\n")
        for f in filesToTransfer {
            let sizeStr = CrocUtils.byteCountDecimal(f.size)
            writeStatus("  " + S.fg(S.info, S.fileIcon) + " " + f.name + S.fg(S.dim, " (\(sizeStr))") + "\r\n")
        }

        if !options.noPrompt || options.ask || senderInfo.ask {
            phase = .waitingForAcceptance
            let action = senderInfo.sendingText ? "Display text message" : "Accept"
            let accepted: Bool
            if options.ask || senderInfo.ask {
                let remoteMachine = senderInfo.machineID.isEmpty ? "unknown" : senderInfo.machineID
                let fileName = filesToTransfer.first?.name ?? "transfer"
                accepted = await promptYesNo("Your machine id is '\(CrocPresentation.machineID())'.\n\(action) '\(fileName)' (\(CrocUtils.byteCountDecimal(totalSize))) from '\(remoteMachine)'? (Y/n) ")
            } else {
                accepted = await promptYesNo("\(action)? (Y/n) ")
            }

            if !accepted {
                let errorMsg = CrocMessage(type: .error, message: "refusing files")
                try await controlConn.send(try errorMsg.encode(key: key))
                throw CrocError.filesRejected
            }
        }

        let outDir = options.outputFolder

        var fileIndex = 0
        while fileIndex < filesToTransfer.count {
            guard !cancelled else { throw CrocError.transferCancelled }
            let file = filesToTransfer[fileIndex]

            if file.size == 0 || !file.symlink.isEmpty {
                let destPath = (outDir as NSString).appendingPathComponent(
                    file.folderRemote.isEmpty ? file.name : (file.folderRemote as NSString).appendingPathComponent(file.name)
                )
                if !file.symlink.isEmpty {
                    try? FileManager.default.createSymbolicLink(atPath: destPath, withDestinationPath: file.symlink)
                } else {
                    try CrocReceiver.prepareFile(at: destPath, size: 0, mode: file.mode)
                }
                fileIndex += 1
                continue
            }

            let destPath = (outDir as NSString).appendingPathComponent(
                file.folderRemote.isEmpty ? file.name : (file.folderRemote as NSString).appendingPathComponent(file.name)
            )

            // Check if file already exists with matching hash (skip/resume).
            // Matches Go's updateIfRecipientHasFileInfo logic.
            if FileManager.default.fileExists(atPath: destPath),
               let existingAttrs = try? FileManager.default.attributesOfItem(atPath: destPath),
               let existingSize = existingAttrs[.size] as? Int64,
               existingSize == file.size,
               let expectedHash = file.hash, !expectedHash.isEmpty {
                let actualHash = try? CrocHasher.hashFile(at: destPath, algorithm: senderInfo.hashAlgorithm)
                if actualHash == expectedHash {
                    // File is complete — skip it
                    writeStatus("  " + S.fg(S.success, S.checkIcon) + " " + file.name + S.fg(S.dim, " (already complete)") + "\r\n")
                    if let modTime = file.modTime {
                        try? FileManager.default.setAttributes([.modificationDate: modTime], ofItemAtPath: destPath)
                    }
                    fileIndex += 1
                    continue
                } else if !options.overwrite && !options.sendingText {
                    let missingChunks = CrocReceiver.chunkRangesToChunks(
                        CrocReceiver.missingChunks(at: destPath, fileSize: file.size, chunkSize: CrocConstants.chunkSize)
                    )
                    let percentDone = 100 - (Double(missingChunks.count * CrocConstants.chunkSize) / Double(max(file.size, 1)) * 100)
                    let formattedPercent = String(format: "%.1f", percentDone)
                    let prompt = percentDone < 99
                        ? "Resume '\(file.name)' (\(formattedPercent)%)? (y/N)   (use --overwrite to omit) "
                        : "Overwrite '\(file.name)'? (y/N) (use --overwrite to omit) "
                    let accepted = await promptYesNo(prompt, defaultYes: false)
                    if !accepted {
                        writeStatus(S.fg(S.dim, "  Skipping '\(file.name)'") + "\r\n")
                        fileIndex += 1
                        continue
                    }
                }
            }

            try CrocReceiver.prepareFile(at: destPath, size: file.size, mode: file.mode)

            // Calculate missing chunks for resume support
            let chunkRanges = CrocReceiver.missingChunks(at: destPath, fileSize: file.size, chunkSize: CrocConstants.chunkSize)
            currentFileChunks = CrocReceiver.chunkRangesToChunks(chunkRanges).sorted().map { Int64($0) }

            // Open file and set up state for long-lived data tasks.
            // Lock protects against concurrent data task access.
            let handle = FileHandle(forWritingAtPath: destPath)
            try handle?.truncate(atOffset: UInt64(file.size))
            receiverLock.withLock {
                currentFile = handle
                currentFileIsClosed = false
                totalSent = 0
                totalChunksTransferred = 0
                filesToTransferCurrentNum = fileIndex
            }

            // Set up progress bar for this file (data tasks read it via receiverProgressBar)
            let showBar = !options.stdout && !senderInfo.sendingText
            receiverProgressBar = showBar ? CrocProgress(
                filename: file.name,
                totalBytes: file.size,
                fileIndex: fileIndex,
                fileCount: filesToTransfer.count,
                output: output
            ) : nil

            // Send ready with chunk request
            let request = CrocRemoteFileRequest(
                currentFileChunkRanges: chunkRanges,
                filesToTransferCurrentNum: fileIndex,
                machineID: CrocPresentation.machineID()
            )
            let requestData = try JSONEncoder().encode(request)
            let readyMsg = CrocMessage(type: .recipientReady, bytes: requestData)
            try await controlConn.send(try readyMsg.encode(key: key))

            phase = .transferringData(fileIndex: fileIndex, totalFiles: filesToTransfer.count, progress: 0)

            // Wait for the long-lived data tasks to signal file completion.
            // They set currentFileIsClosed = true and send closeSender on control.
            while !currentFileIsClosed && !cancelled {
                try await Task.sleep(for: .milliseconds(50))
            }
            if let dataError = receiverLock.withLock({ receiverDataError }) { throw dataError }
            guard !cancelled else { throw CrocError.transferCancelled }

            receiverProgressBar?.finish()
            receiverProgressBar = nil

            // Wait for close-recipient from sender
            let ackData = try await controlConn.receiveSkippingProbes()
            let ackMsg = try CrocMessage.decode(data: ackData, key: key)
            guard ackMsg.type == .closeRecipient else {
                throw CrocError.unexpectedMessageType(ackMsg.type?.rawValue ?? "nil")
            }

            // Verify hash
            if let expectedHash = file.hash, !expectedHash.isEmpty {
                let actualHash = try CrocHasher.hashFile(at: destPath, algorithm: senderInfo.hashAlgorithm)
                if actualHash != expectedHash {
                    throw CrocError.hashMismatch(filename: file.name)
                }
            }

            // Set modification time
            if let modTime = file.modTime {
                try? FileManager.default.setAttributes([.modificationDate: modTime], ofItemAtPath: destPath)
            }

            if options.stdout || senderInfo.sendingText {
                let fileData = try Data(contentsOf: URL(fileURLWithPath: destPath))
                outputData(fileData)
                try? FileManager.default.removeItem(atPath: destPath)
            }

            if file.tempFile, file.name.hasSuffix(".zip") {
                do {
                    try CrocFileUtils.unzipFile(source: destPath, destination: outDir)
                    try? FileManager.default.removeItem(atPath: destPath)
                } catch {
                    writeDebug("Failed to unzip temporary archive \(file.name): \(error.localizedDescription)")
                }
            }

            fileIndex += 1
        }

        // Create empty folders
        for folder in emptyFoldersToTransfer {
            let folderPath = (outDir as NSString).appendingPathComponent(folder.folderRemote)
            try? FileManager.default.createDirectory(atPath: folderPath, withIntermediateDirectories: true)
        }

        let finishedMsg = CrocMessage(type: .finished)
        try await controlConn.send(try finishedMsg.encode(key: key))

        let finishedAckData = try await controlConn.receiveSkippingProbes()
        let finishedAck = try CrocMessage.decode(data: finishedAckData, key: key)
        guard finishedAck.type == .finished else {
            throw CrocError.unexpectedMessageType(finishedAck.type?.rawValue ?? "nil")
        }

        let duration = Date().timeIntervalSince(startTime ?? Date())
        phase = .completed(totalBytes: totalSent, duration: duration)
        writeStatus(transferCompleteMessage(totalBytes: totalSent, duration: duration))
    }

    private func transferCompleteMessage(totalBytes: Int64, duration: TimeInterval) -> String {
        let speed = duration > 0 ? Double(totalBytes) / duration : 0
        let speedStr = CrocUtils.byteCountDecimal(Int64(speed)) + "/s"
        let sizeStr = CrocUtils.byteCountDecimal(totalBytes)
        let timeStr = String(format: "%.1fs", duration)
        return "\r\n" + S.fg(S.success, "🐊 Transfer complete")
             + S.fg(S.dim, " (\(sizeStr) in \(timeStr), \(speedStr))") + "\r\n"
    }

    // MARK: - Long-Lived Receiver Data Tasks

    /// Start long-lived data receiver tasks (one per data port).
    /// Matches Go's `go c.receiveData(j)` goroutines that run for the entire session.
    /// These tasks receive encrypted chunks, decrypt, decompress, and write to the
    /// current file. They check `currentFileIsClosed` to know when a file is done.
    /// Abort a receive from a data task. Closing the connections unblocks both
    /// our control-channel wait and the sender, which never gets `closeSender`.
    private func failReceive(_ error: Error) {
        guard !cancelled else { return }
        receiverLock.withLock { receiverDataError = error }
        cancelled = true
        for conn in connections {
            conn.close()
        }
    }

    private func startReceiverDataTasks(key: Data) {
        // Data connections are at indices 1..N (index 0 is control)
        for j in 0..<(connections.count - 1) {
            let conn = connections[j + 1]
            let task = Task { [weak self] in
                while let self, !self.cancelled {
                    let data: Data
                    do {
                        data = try await conn.receive()
                    } catch {
                        break
                    }
                    // Skip keepalive probes
                    if data.count == 1 && data[0] == 1 { continue }

                    // Decrypt — treat auth failures as fatal (matches Go's panic on decrypt error).
                    // Silent drop via try? would turn key mismatch/tampering into an infinite hang.
                    var payload: Data
                    do {
                        payload = try CrocEncryption.decrypt(data, key: key)
                    } catch {
                        Self.logger.error("data channel decrypt failed: \(error.localizedDescription)")
                        self.failReceive(CrocError.decryptionFailed)
                        break
                    }
                    if !self.options.noCompress {
                        payload = CrocCompression.decompress(payload)
                    }
                    guard payload.count > 8 else { continue }

                    let position = payload.prefix(8).withUnsafeBytes { $0.load(as: UInt64.self) }
                    let posLE = UInt64(littleEndian: position)
                    let fileData = payload.dropFirst(8)

                    // Lock mirrors Go's c.mutex — protects seek+write, counters,
                    // and the completion check from concurrent data tasks.
                    let shouldSignalClose: Bool
                    do {
                        shouldSignalClose = try self.receiverLock.withLock { () throws -> Bool in
                            guard !self.currentFileIsClosed, let handle = self.currentFile else {
                                return false
                            }

                            try handle.seek(toOffset: posLE)
                            try handle.write(contentsOf: fileData)

                            let chunkBytes = Int64(fileData.count)
                            self.totalSent += chunkBytes
                            self.totalChunksTransferred += 1
                            self.receiverProgressBar?.update(bytesAdded: chunkBytes)

                            let expectedSize = self.filesToTransfer[safe: self.filesToTransferCurrentNum]?.size ?? 0
                            let chunksComplete = !self.currentFileChunks.isEmpty
                                && self.totalChunksTransferred == self.currentFileChunks.count
                            let bytesComplete = expectedSize > 0 && self.totalSent >= expectedSize
                            if chunksComplete || bytesComplete {
                                self.currentFileIsClosed = true
                                try? handle.close()
                                return true
                            }

                            return false
                        }
                    } catch {
                        // e.g. ENOSPC; the legacy write API would have raised an ObjC exception.
                        Self.logger.error("data channel file write failed: \(error.localizedDescription)")
                        self.failReceive(CrocError.transferFailed(error.localizedDescription))
                        break
                    }

                    guard shouldSignalClose else {
                        continue
                    }

                    // Send close-sender on control channel outside the lock to avoid deadlock.
                    let closeMsg = CrocMessage(type: .closeSender)
                    try? await self.connections.first?.send(try closeMsg.encode(key: key))
                }
            }
            receiverDataTasks.append(task)
        }
    }

    private var filesToTransferCurrentNum = 0

    // MARK: - Local Relay

    /// Set up local relay servers and advertise the control port on LAN.
    private func startLocalRelay() async throws {
        guard localRelays.isEmpty else { return }

        let ports = findOpenPorts(starting: options.port, count: options.transfers + 1)
        guard ports.count >= options.transfers + 1 else {
            throw CrocError.connectionFailed("not enough open ports to run local relay")
        }

        // Update relayPorts to the actual ports
        options.relayPorts = ports.map { String($0) }
        localRelayPorts = options.relayPorts

        // Banner = comma-separated transfer ports (indices 1..N)
        let banner = options.relayPorts.dropFirst().joined(separator: ",")

        // Start a relay server on EVERY port (matching Go's setupLocalRelay)
        for port in ports {
            let relay = CrocRelayServer(
                host: "0.0.0.0",
                port: UInt16(port),
                password: options.relayPassword,
                banner: banner
            )
            localRelays.append(relay)
            try await relay.start()
        }

        // Broadcast the first port on LAN for receiver discovery.
        // Matches Go: 30s limit when the public relay is also in play,
        // unlimited when local-only.
        broadcastTask = CrocPeerDiscovery.startBroadcasting(
            port: String(ports[0]),
            multicastAddress: options.multicastAddress,
            timeLimit: options.onlyLocal ? nil : CrocConstants.senderDiscoveryTimeLimit,
            stopSignal: { [weak self] in self?.cancelled ?? true }
        )

        if options.onlyLocal {
            options.relayAddress = "127.0.0.1:\(ports[0])"
        }
    }

    // MARK: - Helpers

    private func findOpenPorts(starting: Int, count: Int) -> [Int] {
        var openPorts: [Int] = []

        for port in starting..<(starting + 200) {
            guard let candidatePort = in_port_t(exactly: port) else { continue }
            let socketFD = socket(AF_INET, SOCK_STREAM, 0)
            if socketFD < 0 {
                continue
            }

            var reuseAddr: Int32 = 1
            _ = withUnsafePointer(to: &reuseAddr) {
                setsockopt(socketFD, SOL_SOCKET, SO_REUSEADDR, $0, socklen_t(MemoryLayout<Int32>.size))
            }

            var address = sockaddr_in()
            address.sin_len = UInt8(MemoryLayout<sockaddr_in>.size)
            address.sin_family = sa_family_t(AF_INET)
            address.sin_port = candidatePort.bigEndian
            address.sin_addr = in_addr(s_addr: INADDR_ANY.bigEndian)

            let bindResult = withUnsafePointer(to: &address) {
                $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                    bind(socketFD, $0, socklen_t(MemoryLayout<sockaddr_in>.size))
                }
            }

            close(socketFD)

            if bindResult == 0 {
                openPorts.append(port)
            }

            if openPorts.count >= count {
                break
            }
        }

        return openPorts
    }

    private func parseRelayPorts(_ banner: String) -> [String] {
        banner
            .components(separatedBy: ",")
            .map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }
            .filter { !$0.isEmpty }
    }

    private func openDataConnections() async throws {
        guard connections.count == 1 else { return }

        let (dataHost, _) = try parseHost(options.relayAddress)
        let relayPassword = options.relayPassword
        let relayPorts = options.relayPorts
        let relayOptions = options
        let orderedConnections = try await withThrowingTaskGroup(of: (Int, CrocComm).self) { group in
            for (index, port) in relayPorts.enumerated() {
                let room = "\(options.roomName)-\(index)"
                group.addTask {
                    let dataRelay = try await CrocRelayClient.connect(
                        to: "\(dataHost):\(port)",
                        password: relayPassword,
                        room: room,
                        options: relayOptions
                    )
                    return (index, dataRelay.comm)
                }
            }

            var conns = Array<CrocComm?>(repeating: nil, count: relayPorts.count)
            for try await (index, conn) in group {
                conns[index] = conn
            }
            return conns.compactMap { $0 }
        }

        connections.append(contentsOf: orderedConnections)
    }

    private func parseHost(_ address: String) throws -> (host: String, port: UInt16) {
        if address.hasPrefix("[") {
            guard let closeBracket = address.firstIndex(of: "]") else {
                throw CrocError.connectionFailed("invalid address")
            }
            let host = String(address[address.index(after: address.startIndex)..<closeBracket])
            return (host, UInt16(CrocConstants.defaultPortInt))
        }
        let parts = address.components(separatedBy: ":")
        return (parts[0], UInt16(parts.count > 1 ? parts[1] : CrocConstants.defaultPort) ?? UInt16(CrocConstants.defaultPortInt))
    }

    private func receiveCommandFlags() -> String {
        var flags: [String] = []
        if !options.relayAddress.isEmpty,
           options.relayAddress != "\(CrocConstants.defaultRelayHost):\(CrocConstants.defaultPort)",
           !options.onlyLocal {
            flags.append("--relay \(options.relayAddress)")
        }
        if options.relayPassword != CrocConstants.defaultPassphrase {
            flags.append("--pass \(options.relayPassword)")
        }
        return flags.isEmpty ? "" : flags.joined(separator: " ") + " "
    }

    private func connectToRelay(room: String) async throws -> CrocRelayClient.RelayConnection {
        let candidates = relayCandidates()
        var lastError: Error?

        for (index, candidate) in candidates.enumerated() {
            if cancelled || Task.isCancelled {
                throw CrocError.cancelled
            }
            phase = .connecting(host: candidate)
            let timeout: TimeInterval
            if index == 0 && candidates.count > 1 {
                timeout = options.isSender ? 0.1 : 0.2
            } else {
                timeout = CrocConstants.relayConnectTimeout
            }
            do {
                let relay = try await CrocRelayClient.connect(
                    to: candidate,
                    password: options.relayPassword,
                    room: room,
                    options: options,
                    timeout: timeout
                )
                if CrocPresentation.looksLikeIPv6Address(candidate) {
                    options.relayAddress6 = candidate
                    if options.relayAddress.isEmpty {
                        options.relayAddress = candidate
                    }
                } else {
                    options.relayAddress = candidate
                }
                writeDebug("Connected to relay candidate \(candidate)")
                if cancelled || Task.isCancelled {
                    relay.comm.close()
                    throw CrocError.cancelled
                }
                return relay
            } catch let error as CrocError where error.isCancellation {
                writeDebug("Relay candidate cancelled: \(candidate)")
                throw error
            } catch is CancellationError {
                writeDebug("Relay candidate cancelled: \(candidate)")
                throw CrocError.cancelled
            } catch {
                if cancelled || Task.isCancelled {
                    throw CrocError.cancelled
                }
                writeDebug("Relay candidate failed: \(candidate) -> \(error.localizedDescription)")
                lastError = error
            }
        }

        throw lastError ?? CrocError.connectionFailed(options.relayAddress)
    }

    private func throwIfCancelled() throws {
        if cancelled || Task.isCancelled {
            throw CrocError.cancelled
        }
    }

    private func relayCandidates() -> [String] {
        var candidates: [String] = []
        if !options.relayAddress6.isEmpty {
            candidates.append(options.relayAddress6)
        }
        if !options.relayAddress.isEmpty {
            candidates.append(options.relayAddress)
        }
        return Array(NSOrderedSet(array: candidates)) as? [String] ?? candidates
    }

    private func prepareSenderControlConnection() async throws -> PreparedRelayConnection {
        var senderOptions = options
        if senderOptions.relayAddress.isEmpty && senderOptions.relayAddress6.isEmpty {
            if senderOptions.onlyLocal, let localPort = localRelayPorts.first {
                senderOptions.relayAddress = "127.0.0.1:\(localPort)"
            } else {
                senderOptions.relayAddress = "\(CrocConstants.defaultRelayHost):\(CrocConstants.defaultPort)"
                senderOptions.relayAddress6 = "\(CrocConstants.defaultRelay6Host):\(CrocConstants.defaultPort)"
            }
        }

        enum CandidateOutcome {
            case success(PreparedRelayConnection)
            case failure(Error)
        }

        return try await withThrowingTaskGroup(of: CandidateOutcome.self) { group in
            var startedCandidates = 0

            if !senderOptions.disableLocal, let localPort = localRelayPorts.first {
                startedCandidates += 1
                group.addTask {
                    do {
                        return .success(try await self.prepareSenderLocalControlConnection(options: senderOptions, port: localPort))
                    } catch {
                        return .failure(error)
                    }
                }
            }

            if !senderOptions.onlyLocal {
                startedCandidates += 1
                group.addTask {
                    do {
                        return .success(try await self.prepareSenderPublicControlConnection(options: senderOptions))
                    } catch {
                        return .failure(error)
                    }
                }
            }

            guard startedCandidates > 0 else {
                throw CrocError.connectionFailed("found no relay candidates")
            }

            var lastError: Error?
            for try await outcome in group {
                switch outcome {
                case .success(let prepared):
                    group.cancelAll()
                    return prepared
                case .failure(let error as CrocError) where error.isCancellation:
                    continue
                case .failure(_ as CancellationError):
                    continue
                case .failure(let error):
                    lastError = error
                }
            }

            throw lastError ?? CrocError.connectionFailed(senderOptions.relayAddress)
        }
    }

    private func prepareSenderPublicControlConnection(options: CrocOptions) async throws -> PreparedRelayConnection {
        let candidates = relayCandidates(for: options)
        var lastError: Error?

        for (index, candidate) in candidates.enumerated() {
            if cancelled || Task.isCancelled {
                throw CrocError.cancelled
            }

            let timeout: TimeInterval
            if index == 0 && candidates.count > 1 {
                timeout = 0.1
            } else {
                timeout = CrocConstants.relayConnectTimeout
            }

            do {
                let relay = try await CrocRelayClient.connect(
                    to: candidate,
                    password: options.relayPassword,
                    room: options.roomName,
                    options: options,
                    timeout: timeout
                )
                try await Self.senderPublicPreTransfer(
                    controlConn: relay.comm,
                    sharedSecret: options.sharedSecret,
                    curve: options.curve,
                    disableLocal: options.disableLocal,
                    localRelayPorts: localRelayPorts,
                    cancelled: { [weak self] in self?.cancelled ?? true }
                )

                let relayPorts = parseRelayPorts(relay.banner)
                let selectedPorts = options.noMultiplexing && relayPorts.count > 1
                    ? [relayPorts[0]]
                    : relayPorts
                return PreparedRelayConnection(
                    comm: relay.comm,
                    relayAddress: candidate,
                    relayPorts: selectedPorts,
                    usingLocalRelay: false,
                    externalIP: relay.remoteIP
                )
            } catch let error as CrocError where error.isCancellation {
                throw error
            } catch is CancellationError {
                throw CrocError.cancelled
            } catch {
                lastError = error
            }
        }

        throw lastError ?? CrocError.connectionFailed(options.relayAddress)
    }

    private func prepareSenderLocalControlConnection(options: CrocOptions, port: String) async throws -> PreparedRelayConnection {
        try await Task.sleep(for: .seconds(CrocConstants.localRelayStartupDelay))

        let relay = try await CrocRelayClient.connect(
            to: "127.0.0.1:\(port)",
            password: options.relayPassword,
            room: options.roomName,
            options: options,
            timeout: CrocConstants.connectionTimeout
        )
        try await Self.waitForLocalHandshake(
            controlConn: relay.comm,
            cancelled: { [weak self] in self?.cancelled ?? true }
        )

        let relayPorts = parseRelayPorts(relay.banner)
        let selectedPorts = options.noMultiplexing && relayPorts.count > 1
            ? [relayPorts[0]]
            : relayPorts
        return PreparedRelayConnection(
            comm: relay.comm,
            relayAddress: "127.0.0.1",
            relayPorts: selectedPorts,
            usingLocalRelay: true,
            externalIP: relay.remoteIP
        )
    }

    private func relayCandidates(for options: CrocOptions) -> [String] {
        var candidates: [String] = []
        if !options.relayAddress6.isEmpty {
            candidates.append(options.relayAddress6)
        }
        if !options.relayAddress.isEmpty {
            candidates.append(options.relayAddress)
        }
        return Array(NSOrderedSet(array: candidates)) as? [String] ?? candidates
    }
}

private extension Array {
    nonisolated subscript(safe index: Int) -> Element? {
        indices.contains(index) ? self[index] : nil
    }
}

#endif
