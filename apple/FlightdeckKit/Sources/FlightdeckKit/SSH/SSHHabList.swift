import Foundation

/// One hab the app reaches over its own SSH carrier. Kept in app storage;
/// the key it names lives in the Keychain.
public struct SSHHabEntry: Codable, Sendable, Equatable, Identifiable {
    public var id: String
    /// The row label; nil uses the hab's host id.
    public var label: String?
    public var user: String
    /// Tried in this order (app.md 10.1).
    public var endpoints: [SSHEndpoint]
    /// The pinned host key's fingerprint. Over the fallback path this is the
    /// system sshd's key, confirmed by the user on first connect.
    public var hostKeyFingerprint: String
    /// The pinned host key as an OpenSSH line, when the app saw it.
    public var hostKey: String?
    /// The device key this entry signs with.
    public var keyID: String
    /// The remote command; nil runs `gff-hab connect`.
    public var command: String?
    /// The hab's device id from a pairing code, when it came from one.
    public var deviceID: String?
    public var added: Date

    public init(id: String = UUID().uuidString, label: String? = nil, user: String, endpoints: [SSHEndpoint], hostKeyFingerprint: String, hostKey: String? = nil, keyID: String = DeviceKeys.defaultID, command: String? = nil, deviceID: String? = nil, added: Date = Date()) {
        self.id = id
        self.label = label
        self.user = user
        self.endpoints = endpoints
        self.hostKeyFingerprint = hostKeyFingerprint
        self.hostKey = hostKey
        self.keyID = keyID
        self.command = command
        self.deviceID = deviceID
        self.added = added
    }

    /// The connection id on the board.
    public var connectionID: String { "nio:" + id }

    /// What a key move records for this entry (transport.md 7.2): the id
    /// and the host key, so an entry pointed at another host waits again.
    public var moveMark: String { id + " " + hostKeyFingerprint }

    /// `user@first-endpoint`.
    public var destination: String {
        user + "@" + (endpoints.first?.description ?? "-")
    }

    /// The carrier for this entry, with the key it names.
    public func transport(keys: DeviceKeys, timing: SSHTiming = SSHTiming()) throws -> Transport {
        guard let key = try keys.signingSource(keyID: keyID, entry: moveMark) else {
            throw TransportError("no key: pair again or choose a key")
        }
        return NIOSSHTransport(user: user, endpoints: endpoints, hostKeyFingerprint: hostKeyFingerprint, key: key, command: command, timing: timing)
    }
}

/// The SSH hab entries, as one JSON file: `Flightdeck/ssh-habs.json` under
/// Application Support, or the path in `$FLIGHTDECK_SSH_HABS`.
public struct SSHHabList: Codable, Sendable, Equatable {
    public var habs: [SSHHabEntry]

    public init(habs: [SSHHabEntry] = []) {
        self.habs = habs
    }

    public static func path(environment env: [String: String] = ProcessInfo.processInfo.environment) -> URL? {
        if let p = env["FLIGHTDECK_SSH_HABS"], !p.isEmpty {
            return URL(fileURLWithPath: (p as NSString).expandingTildeInPath)
        }
        return FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first?
            .appendingPathComponent("Flightdeck/ssh-habs.json")
    }

    /// Reads the file. A missing file is an empty list; a broken one throws.
    public static func load(from url: URL?) throws -> SSHHabList {
        guard let url, FileManager.default.fileExists(atPath: url.path) else { return SSHHabList() }
        return try decode(Data(contentsOf: url))
    }

    /// Writes the file atomically, readable by this user only.
    public func save(to url: URL?) throws {
        guard let url else { throw TransportError("no place to store the hab list") }
        try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys]
        enc.dateEncodingStrategy = .iso8601
        try enc.encode(self).write(to: url, options: [.atomic])
        try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }

    public static func decode(_ data: Data) throws -> SSHHabList {
        let dec = JSONDecoder()
        dec.dateDecodingStrategy = .iso8601
        return try dec.decode(SSHHabList.self, from: data)
    }
}
