import Foundation
import NIOCore
import NIOSSH

/// The device's half of QR pairing (transport.md 6.1.2, app.md 18.8): dial
/// the pairing window with the code's host key pinned, sign in with the
/// device key, open the `flightdeck-pair` subsystem, send the token and the
/// device name, wait for the owner, and turn the answer into a hab entry.
public enum Pairing {
    /// The only subsystem the window serves.
    public static let subsystem = "flightdeck-pair"
    /// The SSH user name at the window. The window takes any name; the token decides.
    public static let user = "flightdeck"
    /// The longest answer line the app reads. The window's lines are far shorter.
    static let maxAnswer = 64 * 1024
    /// The most sshd endpoints and host keys the app keeps from an answer.
    static let maxEndpoints = PairingCode.maxEndpoints
    static let maxHostKeys = 16

    /// What kind of device asks, as the window records it.
    public enum Kind: String, Sendable, Codable {
        case phone, tablet, desktop
    }

    /// How long each step may take.
    public struct Timing: Sendable {
        /// The SSH dial (TCP, key exchange, auth).
        public var ssh = SSHTiming()
        /// From the request sent to the first answer. The window answers at once.
        public var firstAnswer: Duration = .seconds(25)
        /// From `waiting` to the final answer. The window lives at most five
        /// minutes and answers `expired` when it closes.
        public var owner: Duration = .seconds(330)

        public init() {}
    }

    /// The last four characters of a key fingerprint: the code the owner
    /// types on the hab (transport.md 6.1.5).
    public static func confirmCode(_ fingerprint: String) -> String {
        String(fingerprint.suffix(4))
    }

    /// The device name as the window will clean it: no control or format
    /// characters, runs of white space as one space, trimmed, at most 64
    /// characters. Empty becomes "Flightdeck device".
    public static func cleanName(_ s: String) -> String {
        var out = ""
        var space = false
        for u in s.unicodeScalars {
            if CharacterSet.controlCharacters.contains(u) || u.properties.generalCategory == .format {
                continue
            }
            if CharacterSet.whitespacesAndNewlines.contains(u) {
                space = !out.isEmpty
                continue
            }
            if space {
                out.unicodeScalars.append(" ")
                space = false
            }
            out.unicodeScalars.append(u)
        }
        if out.count > 64 { out = String(out.prefix(64)) }
        return out.isEmpty ? "Flightdeck device" : out
    }

    /// The request line, newline included.
    public static func requestLine(token: Data, name: String, kind: Kind) -> Data {
        struct Wire: Encodable {
            var v = 1
            var token: String
            var name: String
            var kind: String
        }
        let enc = JSONEncoder()
        enc.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        var d = (try? enc.encode(Wire(token: PairingCode.base64url(token), name: cleanName(name), kind: kind.rawValue))) ?? Data()
        d.append(0x0A)
        return d
    }

    /// One answer line from the window.
    public enum Answer: Equatable, Sendable {
        case waiting
        case paired(Paired)
        case refused(String)

        /// Parses one line (no newline). Throws `PairingFailed` for anything
        /// that is not a known answer of format 1.
        public static func parse(_ line: Data) throws -> Answer {
            guard line.count <= Pairing.maxAnswer else { throw PairingFailed("the hab's answer is too long") }
            struct Head: Decodable {
                var v: Int?
                var state: String?
                var reason: String?
            }
            let dec = JSONDecoder()
            guard let head = try? dec.decode(Head.self, from: line) else { throw PairingFailed("the hab's answer is not JSON") }
            guard head.v == 1 else { throw PairingFailed("the hab answered in an unknown format") }
            switch head.state {
            case "waiting":
                return .waiting
            case "refused":
                return .refused(head.reason ?? "")
            case "paired":
                do {
                    return .paired(try dec.decode(Paired.self, from: line))
                } catch {
                    throw PairingFailed("the hab's paired answer is incomplete")
                }
            default:
                throw PairingFailed("the hab answered with an unknown state")
            }
        }
    }

    /// The `paired` answer.
    public struct Paired: Decodable, Equatable, Sendable {
        public struct Hab: Decodable, Equatable, Sendable {
            public var id: String
            public var label: String?
        }
        public struct SSH: Decodable, Equatable, Sendable {
            public var endpoints: [String]
            public var host_keys: [String]?
        }
        public var hab: Hab
        public var device_id: String
        public var user: String
        public var ssh: SSH
        public var forced_command: Bool?
    }

    /// The hab entry a pairing produced, before it is tested and stored.
    public struct Result: Sendable, Equatable {
        /// The entry. With no usable host key in the answer its pinned key
        /// is empty: the app must probe and have the owner trust it.
        public var entry: SSHHabEntry
        /// The answer carried no usable sshd host key (app.md 18.8 step 6).
        public var needsHostKey: Bool { entry.hostKeyFingerprint.isEmpty }
    }

    /// Checks a `paired` answer against the code and the key that signed,
    /// and builds the entry (app.md 18.8 step 4). Everything in the answer
    /// came from the window the code pinned, but it is still checked before
    /// any of it is stored.
    public static func entry(from p: Paired, code: PairingCode, deviceFingerprint: String, keyID: String, now: Date = Date()) throws -> Result {
        guard p.hab.id == code.deviceID else {
            throw PairingFailed("the hab answered with another hab id")
        }
        guard p.device_id == deviceFingerprint else {
            throw PairingFailed("the hab admitted another key than this device's")
        }
        let user = p.user
        guard (1...64).contains(user.count),
              !user.hasPrefix("-"),
              user.unicodeScalars.allSatisfy({ $0.isASCII && $0.value > 0x20 && $0.value < 0x7F && $0 != "@" && $0 != "\"" && $0 != "'" && $0 != "\\" })
        else {
            throw PairingFailed("the hab answered with a user name the app cannot use")
        }
        guard p.ssh.endpoints.count <= maxEndpoints else {
            throw PairingFailed("the hab answered with too many addresses")
        }
        var eps: [SSHEndpoint] = []
        for e in p.ssh.endpoints {
            guard let ep = SSHEndpoint.parse(e) else { throw PairingFailed("the hab answered with a bad address") }
            if !eps.contains(ep) { eps.append(ep) }
        }
        guard !eps.isEmpty else { throw PairingFailed("the hab answered with no SSH address") }

        // The first ssh-ed25519 key, else the first ECDSA key. Lines that do
        // not parse, or of another type, are skipped.
        var chosen: NIOSSHPublicKey?
        var ed: NIOSSHPublicKey?
        for line in (p.ssh.host_keys ?? []).prefix(maxHostKeys) {
            guard line.count <= 1024 else { continue }
            let type = line.split(separator: " ", maxSplits: 1).first.map(String.init) ?? ""
            guard type == "ssh-ed25519" || type.hasPrefix("ecdsa-sha2-nistp"),
                  let k = try? NIOSSHPublicKey(openSSHPublicKey: line) else { continue }
            if chosen == nil { chosen = k }
            if ed == nil && type == "ssh-ed25519" { ed = k }
        }
        let pin = ed ?? chosen

        var label = code.label
        if label == nil, let l = p.hab.label {
            let c = cleanName(l)
            label = c == "Flightdeck device" ? nil : c
        }
        let entry = SSHHabEntry(
            label: label,
            user: user,
            endpoints: eps,
            hostKeyFingerprint: pin.map { SSHFingerprint.of($0) } ?? "",
            hostKey: pin.map { String(openSSHPublicKey: $0) },
            keyID: keyID,
            command: nil,
            deviceID: code.deviceID,
            added: now
        )
        return Result(entry: entry)
    }

    /// Runs the exchange against the code's window. `waiting` gets the
    /// fingerprint of the key the window saw, once the owner is asked; the
    /// app shows its last four characters. Cancelling the task closes the
    /// connection.
    public static func pair(
        _ code: PairingCode,
        key: SSHKeySource,
        keyID: String,
        name: String,
        kind: Kind,
        timing: Timing = Timing(),
        waiting: @escaping @Sendable (String) -> Void
    ) async throws -> Result {
        // One key only: the window admits the first key it sees, so the
        // shown code and the admitted key are one. During a key move this is
        // the new key (transport.md 7.2).
        let signing: (fingerprint: String, key: NIOSSHPrivateKey)
        do {
            guard let first = try await key.signingKeys().first else { throw TransportError("no key to sign with") }
            signing = first
        } catch {
            throw DeviceKeys.explain(error)
        }
        let conn: SSHConnection
        do {
            conn = try await SSHDial.first(code.endpoints, stagger: timing.ssh.stagger) { ep in
                try await SSHConnection.dial(
                    ep, user: user,
                    auth: ClientKeyAuth(user: user, fingerprint: signing.fingerprint, keys: [signing]),
                    check: .pinned(code.deviceID), timing: timing.ssh
                )
            }
        } catch let e as HostKeyChanged {
            throw PairingHostKeyMismatch(endpoint: e.endpoint, presented: e.presented)
        }
        defer { conn.close() }
        // The subsystem request and the request line share the first
        // answer's bound: a window that takes auth and then says nothing is
        // closed.
        let late = Flag()
        let opening = Task {
            try await Task.sleep(for: timing.firstAnswer)
            late.set()
            conn.close()
        }
        return try await withTaskCancellationHandler {
            let stream: SSHChannelStream
            do {
                stream = try await conn.openSession(.subsystem(subsystem), onEnd: {}, onFirstData: nil)
                defer { opening.cancel() }
                try await stream.write(requestLine(token: code.token, name: name, kind: kind))
            } catch {
                opening.cancel()
                if Task.isCancelled { throw CancellationError() }
                if late.isSet { throw PairingFailed("the hab did not answer in time") }
                throw error
            }
            defer { stream.close() }
            var reader = LineReader(stream: stream)
            var answer = try await reader.next(within: timing.firstAnswer)
            if answer == .waiting {
                waiting(signing.fingerprint)
                answer = try await reader.next(within: timing.owner)
            }
            switch answer {
            case .paired(let p):
                return try entry(from: p, code: code, deviceFingerprint: signing.fingerprint, keyID: keyID)
            case .refused(let r):
                throw PairingRefused(reason: r)
            case .waiting:
                throw PairingFailed("the hab said waiting twice")
            }
        } onCancel: {
            conn.close()
        }
    }

    /// Reads answer lines from the subsystem's output.
    struct LineReader {
        let stream: SSHChannelStream
        var buffer = Data()

        init(stream: SSHChannelStream) {
            self.stream = stream
        }

        mutating func next(within limit: Duration) async throws -> Answer {
            let s = stream
            let fired = Flag()
            let timer = Task {
                try await Task.sleep(for: limit)
                fired.set()
                s.close()
            }
            defer { timer.cancel() }
            while true {
                if let nl = buffer.firstIndex(of: 0x0A) {
                    let line = buffer[buffer.startIndex..<nl]
                    buffer = Data(buffer[buffer.index(after: nl)...])
                    return try Answer.parse(Data(line))
                }
                guard buffer.count <= Pairing.maxAnswer else { throw PairingFailed("the hab's answer is too long") }
                guard let d = try await stream.read() else {
                    if fired.isSet { throw PairingFailed("the hab did not answer in time") }
                    throw PairingFailed(buffer.isEmpty ? "the hab closed the connection with no answer" : "the hab closed the connection mid-answer")
                }
                buffer.append(d)
            }
        }
    }

    final class Flag: @unchecked Sendable {
        private let lock = NSLock()
        private var value = false
        func set() { lock.withLock { value = true } }
        var isSet: Bool { lock.withLock { value } }
    }
}

/// The window refused the request, with its reason word (transport.md 6.1.2).
public struct PairingRefused: Error, Equatable, CustomStringConvertible, Sendable {
    public var reason: String

    /// One line per reason (app.md 18.8 step 5).
    public var description: String {
        switch reason {
        case "token": return "This code is not valid on this hab."
        case "expired": return "The code expired: run gff-hab pair again."
        case "declined": return "Declined on the hab."
        case "busy": return "Another device is pairing: try again."
        case "version": return "Update Flightdeck."
        case "failed": return "The hab could not save the key: see its terminal."
        default: return "Pairing failed."
        }
    }
}

/// The exchange broke: a bad or missing answer, a timeout, a dropped
/// connection.
public struct PairingFailed: Error, Equatable, CustomStringConvertible, Sendable {
    public var detail: String
    init(_ detail: String) { self.detail = detail }
    public var description: String { "Pairing failed: \(detail)." }
}

/// The host at a code's address presented another key than the code names.
public struct PairingHostKeyMismatch: Error, Equatable, CustomStringConvertible, Sendable {
    public var endpoint: String
    public var presented: String
    public var description: String {
        "The code does not match the hab at \(endpoint): it presented \(presented). Scan the code again on the hab you mean."
    }
}
