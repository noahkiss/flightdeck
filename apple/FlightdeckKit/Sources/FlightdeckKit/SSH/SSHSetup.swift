import Foundation
import HabAPI
import LocalAuthentication
import NIOSSH
import Observation
import os

/// The in-app SSH carrier as the app sees it: the device key, the hab entries
/// and the flows that change them (app.md 10.1, 10.4, 18.8).
@MainActor
@Observable
public final class SSHSetup {
    public let keys: DeviceKeys
    public private(set) var entries: [SSHHabEntry] = []
    /// The default device key, once loaded or made.
    public private(set) var deviceKey: GeneratedKey?
    /// Why the key could not be loaded or made, for Settings.
    public private(set) var keyError: String?
    /// A pairing offer opened from a link, waiting for its sheet.
    public var pendingCode: PairingCode?
    /// A pairing link that did not parse, for its sheet.
    public var pendingCodeError: String?
    /// Imported OpenSSH keys (app.md 10.4), oldest first.
    public private(set) var importedKeys: [ImportedKey] = []
    /// Why the imported keys could not be listed (a locked device).
    public private(set) var importedKeysError: String?
    /// A key file opened from the share sheet or Files, waiting for the import sheet.
    public var pendingKeyFile: PendingKeyFile?
    /// "Require Face ID for SSH keys": one setting for the generated key and
    /// every imported key (app.md 10.7).
    public private(set) var faceID = false
    /// A key move that waits for hosts to learn the new key (transport.md 7.2).
    public private(set) var keyMove: KeyMove?
    /// Whether an earlier build's key, which signs only while the device is
    /// unlocked, moves to a new key. On by default on iOS, where a Watch
    /// answers while the phone is locked; tests set it.
    @ObservationIgnored public var movesOldKey: Bool = {
        #if os(iOS)
        return true
        #else
        return false
        #endif
    }()

    @ObservationIgnored private let listURL: URL?
    /// Whether the hab list read cleanly when this setup was made. A key
    /// move finishes only then: an unreadable list says nothing about which
    /// hosts still need the old key (transport.md 7.2).
    @ObservationIgnored private var listLoaded = false
    @ObservationIgnored private weak var fleet: Fleet?
    @ObservationIgnored private let timing: SSHTiming
    @ObservationIgnored private let clientLabel: String
    @ObservationIgnored private let clientVersion: String
    @ObservationIgnored private let log = Logger(subsystem: "com.flightdeck.app", category: "ssh")
    /// Asks the device owner to authenticate (Face ID, Touch ID or the
    /// passcode) with this reason. Tests replace it.
    @ObservationIgnored public var authenticate: @Sendable (String) async throws -> Void = { reason in
        _ = try await LAContext().evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason)
    }

    public init(keys: DeviceKeys, listURL: URL?, fleet: Fleet?, timing: SSHTiming = SSHTiming(), clientLabel: String = "phone", clientVersion: String = "0") {
        self.keys = keys
        self.listURL = listURL
        self.fleet = fleet
        self.timing = timing
        self.clientLabel = clientLabel
        self.clientVersion = clientVersion
        do {
            entries = try SSHHabList.load(from: listURL).habs
            listLoaded = true
        } catch {
            log.error("ssh hab list unreadable: \(String(describing: error), privacy: .public)")
        }
        keys.onKeyMove = { [weak self] in
            Task { @MainActor in self?.refreshKeyMove() }
        }
    }

    /// Loads the default key, making it on first use. A locked device leaves
    /// it nil with the reason in `keyError`; call again later.
    @discardableResult
    public func loadKey() -> GeneratedKey? {
        if let k = deviceKey { return k }
        // Only iOS signs while locked; the Mac keeps its items as they are.
        #if os(iOS)
        keys.upgradeStore()
        #endif
        do {
            let k = try keys.defaultKey()
            deviceKey = k
            faceID = try keys.faceID()
            keyError = nil
            // The public key only, so a host can be set up from the log too.
            let line = k.authorizedKeysLine(comment: DeviceKeys.comment)
            log.notice("device key \(k.kind.rawValue, privacy: .public) \(k.fingerprint, privacy: .public)")
            #if DEBUG
            log.debug("device key line: \(line, privacy: .public)")
            print("flightdeck: device key: \(line)")
            #endif
            if movesOldKey {
                do {
                    try keys.startKeyMove()
                } catch {
                    log.error("key move: \(String(describing: error), privacy: .public)")
                }
                refreshKeyMove()
            }
            return deviceKey
        } catch {
            keyError = DeviceKeys.explain(error).description
            return nil
        }
    }

    /// The line to add to a host's authorized keys: during a key move, the
    /// new key's, so a host added now gets the key that stays.
    public var authorizedKeysLine: String? {
        keyMove?.line ?? deviceKey?.authorizedKeysLine(comment: DeviceKeys.comment)
    }

    // MARK: - Key move (transport.md 7.2)

    /// Reads the move's state. Once every entry on the default key has
    /// signed in with the new key, the new key becomes the default key and
    /// those entries reconnect with it alone. That needs the hab list read
    /// from its file now, and read cleanly at start too: when either read
    /// fails, both keys stay and a later call tries again.
    public func refreshKeyMove() {
        guard let next = try? keys.nextKey() else {
            keyMove = nil
            return
        }
        let moved = (try? keys.movedEntries()) ?? []
        var fresh: [SSHHabEntry]?
        if listLoaded {
            do {
                fresh = try SSHHabList.load(from: listURL).habs
            } catch {
                log.error("key move: hab list unreadable, both keys kept: \(String(describing: error), privacy: .public)")
            }
        }
        let waiting = (fresh ?? entries).filter { $0.keyID == DeviceKeys.defaultID && !moved.contains($0.moveMark) }
        guard fresh != nil, waiting.isEmpty else {
            keyMove = KeyMove(line: next.authorizedKeysLine(comment: DeviceKeys.comment), fingerprint: next.fingerprint, kind: next.kind, waiting: waiting)
            return
        }
        do {
            try keys.finishKeyMove()
            deviceKey = try keys.defaultKey()
            keyMove = nil
            reconnect { $0.keyID == DeviceKeys.defaultID }
        } catch {
            log.error("key move finish: \(String(describing: error), privacy: .public)")
        }
    }

    /// Adds a connection to the fleet for every entry.
    public func connectAll() {
        for e in entries { attach(e) }
    }

    private func attach(_ e: SSHHabEntry) {
        guard let fleet else { return }
        let t: Transport
        do {
            t = try e.transport(keys: keys, timing: timing)
        } catch {
            t = BrokenTransport(endpoint: "ssh " + e.destination, error: String(describing: error))
        }
        fleet.add(HabConnection(id: e.connectionID, transport: t, clientLabel: clientLabel, clientVersion: clientVersion), label: e.label)
    }

    // MARK: - Add a hab by hand (app.md 18.8)

    /// Connects with nothing pinned and returns the host key to confirm.
    /// The probe never signs, so any key will do.
    public func probe(_ dest: SSHDestination, keyID: String = DeviceKeys.defaultID) async throws -> NIOSSHPublicKey {
        let key: any SSHKeySource
        do {
            key = try self.key(keyID)
        } catch let e as MissingKey {
            throw e
        } catch {
            throw DeviceKeys.explain(error)
        }
        return try await NIOSSHTransport.probeHostKey(dest.endpoint, user: dest.user, key: key, timing: timing)
    }

    /// Checks an entry with one `hab.ping` over its own connection.
    public func test(_ e: SSHHabEntry) async throws -> Duration {
        let t = try e.transport(keys: keys, timing: timing)
        return try await HabOneShot.call(t, HabMethods.habPing, PingParams(), clientLabel: clientLabel, clientVersion: clientVersion).elapsed
    }

    /// Stores an entry and connects it.
    public func add(_ e: SSHHabEntry) throws {
        var next = entries
        next.removeAll { $0.id == e.id }
        next.append(e)
        try SSHHabList(habs: next).save(to: listURL)
        entries = next
        attach(e)
        refreshKeyMove()
    }

    /// Forgets an entry and drops its connection.
    public func remove(_ e: SSHHabEntry) async throws {
        let next = entries.filter { $0.id != e.id }
        try SSHHabList(habs: next).save(to: listURL)
        entries = next
        await fleet?.remove(id: e.connectionID)
        refreshKeyMove()
    }

    /// Makes a new default key. Every host must get the new public key.
    public func replaceKey() throws {
        let k = try keys.create(id: DeviceKeys.defaultID)
        // The new key signs while locked: no move is needed any more.
        try keys.cancelKeyMove()
        keyMove = nil
        deviceKey = k
        keyError = nil
        // Running connections hold the old key; rebuild them.
        let current = entries
        Task {
            for e in current { await fleet?.remove(id: e.connectionID) }
            for e in current { attach(e) }
        }
    }

    // MARK: - Imported keys (app.md 10.4)

    /// Reads the imported keys from the store.
    public func loadImportedKeys() {
        do {
            importedKeys = try keys.importedKeys()
            faceID = try keys.faceID()
            importedKeysError = nil
        } catch {
            importedKeysError = DeviceKeys.explain(error).description
        }
    }

    /// Decrypts a key file and stores the key. The passphrase and the file's
    /// bytes stay with the caller, which drops them; only the raw key reaches
    /// the Keychain. Decryption runs off the main actor.
    public func importKey(_ file: OpenSSHKeyFile, passphrase: String?) async throws -> (key: ImportedKey, replaced: Bool) {
        let keys = self.keys
        let work = Task.detached(priority: .userInitiated) { () throws -> (key: ImportedKey, replaced: Bool) in
            let m = try file.decrypt(passphrase: passphrase)
            try Task.checkCancellation()
            return try keys.importKey(m)
        }
        // A dismissed sheet cancels its task; pass that on to the derivation.
        let r = try await withTaskCancellationHandler {
            try await work.value
        } onCancel: {
            work.cancel()
        }
        // The fingerprint only: the key never goes to a log.
        log.notice("imported key \(r.key.type.rawValue, privacy: .public) \(r.key.fingerprint, privacy: .public)")
        loadImportedKeys()
        return r
    }

    /// Turns "Require Face ID for SSH keys" on or off. Turning it off asks
    /// the owner to authenticate first, so whoever holds the unlocked device
    /// cannot drop the check; turning it on asks nothing. Keys read the
    /// setting at each dial, so live connections keep running and the next
    /// dial follows it: no burst of Face ID prompts, one per hab (review S1).
    public func setFaceID(_ on: Bool) async throws {
        let current = (try? keys.faceID()) ?? faceID
        guard on != current else { return }
        if !on {
            try await authenticate("Turn off Face ID for SSH keys")
        }
        try keys.setFaceID(on)
        faceID = on
    }

    /// Removes an imported key. Entries that used it say "no key" until one
    /// is chosen.
    public func deleteImportedKey(_ id: String) throws {
        try keys.delete(id)
        loadImportedKeys()
        reconnect { $0.keyID == id }
    }

    /// The hab entries that sign with this key.
    public func entries(using keyID: String) -> [SSHHabEntry] {
        entries.filter { $0.keyID == keyID }
    }

    /// Whether the key an entry names is usable: missing after a restore from
    /// a backup (which carries no keys), unavailable while the device is locked.
    public func keyState(_ e: SSHHabEntry) -> KeyState {
        do {
            _ = try key(e.keyID)
            return .present
        } catch is MissingKey {
            return .missing
        } catch {
            return .unavailable(DeviceKeys.explain(error).description)
        }
    }

    /// Points an entry at another key and reconnects it.
    public func setKey(_ keyID: String, for e: SSHHabEntry) throws {
        guard let i = entries.firstIndex(where: { $0.id == e.id }) else { return }
        var next = entries
        next[i].keyID = keyID
        try SSHHabList(habs: next).save(to: listURL)
        entries = next
        reconnect { $0.id == e.id }
        refreshKeyMove()
    }

    /// The key with this id: the generated one or an imported one. Throws
    /// `MissingKey` when there is none, and the store's error otherwise.
    public func key(_ id: String) throws -> any SSHKeySource {
        if id == DeviceKeys.defaultID {
            if let k = loadKey() { return k }
            throw TransportError(keyError ?? "no device key")
        }
        guard let k = try keys.key(id) else { throw MissingKey() }
        return k
    }

    /// Takes a key file opened from another app, for the import sheet. A
    /// copy iOS placed in the app's Inbox is removed once read.
    public func openKeyFile(_ url: URL) {
        let scoped = url.startAccessingSecurityScopedResource()
        defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        do {
            // At most 64 KiB: "Open in" may hand over any file, a video too.
            let data = try OpenSSHKeyFile.read(contentsOf: url)
            pendingKeyFile = PendingKeyFile(name: url.lastPathComponent, data: data)
        } catch let e as KeyImportError {
            pendingKeyFile = PendingKeyFile(name: url.lastPathComponent, error: e.description)
        } catch {
            pendingKeyFile = PendingKeyFile(name: url.lastPathComponent, error: "Could not read the file: \(error.localizedDescription)")
        }
        #if os(iOS)
        // "Open in" copies the file into this app's own Inbox; drop that copy.
        if Self.isOwnInboxCopy(url, home: NSHomeDirectory()) {
            try? FileManager.default.removeItem(at: url)
        }
        #endif
    }

    /// Whether a file is the copy iOS put in this app's Inbox. Both paths are
    /// standardized with symbolic links resolved, since a file URL may arrive
    /// as /private/var/... while the home folder reads /var/....
    nonisolated static func isOwnInboxCopy(_ url: URL, home: String) -> Bool {
        func clean(_ u: URL) -> String { u.standardizedFileURL.resolvingSymlinksInPath().path }
        let path = clean(url)
        var root = clean(URL(fileURLWithPath: home, isDirectory: true))
        if !root.hasSuffix("/") { root += "/" }
        guard path.hasPrefix(root) else { return false }
        let inside = String(path.dropFirst(root.count))
        return inside.hasPrefix("Documents/Inbox/") || (inside.hasPrefix("tmp/") && inside.contains("-Inbox/"))
    }

    private func reconnect(_ match: (SSHHabEntry) -> Bool) {
        let affected = entries.filter(match)
        guard !affected.isEmpty else { return }
        Task {
            for e in affected { await fleet?.remove(id: e.connectionID) }
            for e in affected { attach(e) }
        }
    }

    // MARK: - QR pairing (app.md 18.8)

    /// Runs the pairing exchange with a code. `waiting` gets the fingerprint
    /// whose last four characters the owner types on the hab. The result is
    /// not stored yet: `finishPairing` tests and stores it.
    public func pair(_ code: PairingCode, name: String, kind: Pairing.Kind, keyID: String = DeviceKeys.defaultID, waiting: @escaping @Sendable (String) -> Void) async throws -> Pairing.Result {
        let key: any SSHKeySource
        do {
            // During a key move the default key offers the new key first.
            if keyID == DeviceKeys.defaultID, loadKey() != nil, let src = try keys.signingSource(keyID: keyID, entry: "") {
                key = src
            } else {
                key = try self.key(keyID)
            }
        } catch let e as MissingKey {
            throw e
        } catch {
            throw DeviceKeys.explain(error)
        }
        var t = Pairing.Timing()
        t.ssh = timing
        let r = try await Pairing.pair(code, key: key, keyID: keyID, name: name, kind: kind, timing: t, waiting: waiting)
        log.notice("paired with \(code.deviceID, privacy: .public)")
        return r
    }

    /// A paired hab whose answer had no sshd host key, waiting for the owner
    /// to trust one. The hab already holds this device's key and the token is
    /// spent, so Settings offers to finish it. Kept in memory only.
    public var unfinishedPairing: SSHHabEntry?

    /// Probes each paired endpoint until one presents a host key.
    public func probePairing(_ entry: SSHHabEntry) async throws -> NIOSSHPublicKey {
        var last: Error = TransportError("the hab has no SSH address")
        for endpoint in entry.endpoints {
            do {
                return try await probe(SSHDestination(user: entry.user, endpoint: endpoint), keyID: entry.keyID)
            } catch {
                if Task.isCancelled { throw CancellationError() }
                last = error
            }
        }
        throw last
    }

    /// Stores a paired entry and connects it, then tests it with one
    /// `hab.ping`. The entry is stored first: the hab already admitted the key,
    /// so a failed first connection must not lose it (the hab refuses to pair
    /// the same key again). A failed test throws with the entry kept.
    public func finishPairing(_ e: SSHHabEntry) async throws {
        guard SSHFingerprint.isValid(e.hostKeyFingerprint) else { throw TransportError("no trusted host key") }
        unfinishedPairing = e
        if entry(id: e.id) != e { try add(e) }
        if unfinishedPairing?.id == e.id { unfinishedPairing = nil }
        _ = try await test(e)
    }

    // MARK: - Password setup (transport.md 6.1.6)

    /// Adds this device's key to a host with the account password, once,
    /// over the host key the owner trusted. The password is not kept.
    public func installKey(_ dest: SSHDestination, hostKeyFingerprint: String, password: String, keyID: String, command: String?, deviceName: String) async throws -> PasswordSetup.Outcome {
        let pub: NIOSSHPublicKey
        if keyID == DeviceKeys.defaultID {
            guard loadKey() != nil else { throw TransportError(keyError ?? "no device key") }
            // During a key move, the key that stays.
            pub = (try? keys.nextKey())?.publicKey ?? deviceKey!.publicKey
        } else {
            pub = try key(keyID).publicKey
        }
        let line = PasswordSetup.keyLine(publicKey: pub, command: try PasswordSetup.forcedCommand(command), name: deviceName)
        let r = try await PasswordSetup.install(dest, hostKeyFingerprint: hostKeyFingerprint, password: password, line: line, key: pub, timing: timing)
        log.notice("password setup on \(dest.endpoint.description, privacy: .public): \(r == .added ? "added" : "present", privacy: .public)")
        return r
    }

    // MARK: - Pairing links

    /// Opens a `flightdeck://pair` link: parses it and shows the pairing sheet.
    public func open(url: URL) {
        do {
            pendingCode = try PairingCode.parse(url.absoluteString)
            pendingCodeError = nil
        } catch {
            pendingCode = nil
            pendingCodeError = String(describing: error)
        }
    }

    // MARK: - One call from the background

    /// Runs one call on the entry's own connection: the call site for
    /// notification actions (a ping now, `approval.answer` later).
    public nonisolated static func perform<P: Encodable & Sendable, R: Decodable & Sendable>(
        entry: SSHHabEntry,
        keys: DeviceKeys,
        _ method: HabMethod<P, R>,
        _ params: P,
        clientLabel: String = "phone",
        deadline: Duration = .seconds(20)
    ) async throws -> HabOneShot.Result<R> {
        let t = try entry.transport(keys: keys)
        return try await HabOneShot.call(t, method, params, clientLabel: clientLabel, deadline: deadline)
    }

    public func entry(id: String) -> SSHHabEntry? {
        entries.first { $0.id == id }
    }
}

/// A key file handed to the app, before the import sheet reads it.
public struct PendingKeyFile: Identifiable, Sendable {
    public let id = UUID()
    public let name: String
    public let data: Data
    /// Why the file could not be read; the sheet shows it instead of parsing.
    public let error: String?

    public init(name: String, data: Data) {
        self.name = name
        self.data = data
        self.error = nil
    }

    public init(name: String, error: String) {
        self.name = name
        self.data = Data()
        self.error = error
    }
}

/// A key move in progress (transport.md 7.2): an earlier build's key signs
/// only while the device is unlocked, so the app made a new one. Each host
/// on the default key needs the new line; the old key works until then.
public struct KeyMove: Equatable, Sendable {
    /// The new key's line for a host's authorized keys.
    public let line: String
    public let fingerprint: String
    public let kind: SSHKeyKind
    /// The entries whose host has not signed in with the new key yet.
    public let waiting: [SSHHabEntry]
}

/// Whether the key a hab entry names can be used now.
public enum KeyState: Equatable, Sendable {
    case present
    /// No such key on this device: deleted, or not carried by a backup.
    case missing
    /// The key exists but cannot be read now, with the reason (a locked device).
    case unavailable(String)
}

/// No key with the id an entry names.
public struct MissingKey: Error, CustomStringConvertible {
    public var description: String { "no key: pair again or choose a key" }
}

/// A transport that always fails with one message: an entry whose key is gone.
struct BrokenTransport: Transport {
    let endpoint: String
    let error: String
    func open(_ kind: StreamKind) async throws -> ByteStream {
        throw TransportError(error)
    }
}
