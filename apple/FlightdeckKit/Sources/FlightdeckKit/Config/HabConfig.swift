import Foundation

/// The habs the Mac app connects to, from Flightdeck's own config file.
///
/// The file is `Flightdeck/habs.json` under Application Support, or the path in
/// `$FLIGHTDECK_HABS`. With no file, the Mac app connects to the hab on its
/// own host over the local socket. Example:
///
/// ```json
/// {"habs": [
///   {"label": "desk", "local": {}},
///   {"label": "build box", "ssh": {"host": "buildbox"}}
/// ]}
/// ```
public struct HabListConfig: Codable, Sendable, Equatable {
    public var habs: [HabConfig]

    public init(habs: [HabConfig]) {
        self.habs = habs
    }

    /// The list with no file: the local hab only.
    public static let localOnly = HabListConfig(habs: [HabConfig(local: .init())])

    /// Where the file is read from.
    public static func path(environment env: [String: String] = ProcessInfo.processInfo.environment) -> URL? {
        if let p = env["FLIGHTDECK_HABS"], !p.isEmpty {
            return URL(fileURLWithPath: (p as NSString).expandingTildeInPath)
        }
        return FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first?
            .appendingPathComponent("Flightdeck/habs.json")
    }

    /// Reads the file. A missing file gives `localOnly`; a broken one throws.
    public static func load(environment env: [String: String] = ProcessInfo.processInfo.environment) throws -> HabListConfig {
        guard let url = path(environment: env), FileManager.default.fileExists(atPath: url.path) else {
            return .localOnly
        }
        return try decode(Data(contentsOf: url))
    }

    public static func decode(_ data: Data) throws -> HabListConfig {
        let c = try JSONDecoder().decode(HabListConfig.self, from: data)
        for (i, h) in c.habs.enumerated() where (h.local == nil) == (h.ssh == nil) {
            throw TransportError("hab \(i + 1) in the config needs exactly one of \"local\" and \"ssh\"")
        }
        return c
    }
}

public struct HabConfig: Codable, Sendable, Equatable {
    /// The row label; nil uses the hab's host id.
    public var label: String?
    public var local: Local?
    public var ssh: SSH?

    public struct Local: Codable, Sendable, Equatable {
        /// The socket; nil uses gff-hab's default (`UnixSocketTransport.defaultPath`).
        public var socket: String?
        public init(socket: String? = nil) { self.socket = socket }
    }

    public struct SSH: Codable, Sendable, Equatable {
        public var host: String
        /// The remote command; nil runs `gff-hab connect`.
        public var command: [String]?
        /// Extra ssh options, such as `["-p", "2222"]`.
        public var options: [String]?
        public init(host: String, command: [String]? = nil, options: [String]? = nil) {
            self.host = host
            self.command = command
            self.options = options
        }
    }

    public init(label: String? = nil, local: Local? = nil, ssh: SSH? = nil) {
        self.label = label
        self.local = local
        self.ssh = ssh
    }

    /// A stable id for the hab within this list.
    public func id(index: Int) -> String {
        if let s = ssh { return "ssh:" + s.host }
        return "local:\(local?.socket ?? "default")#\(index)"
    }

    /// The transport for this entry, or an error when the platform has none.
    public func transport(environment env: [String: String] = ProcessInfo.processInfo.environment) throws -> Transport {
        if let l = local {
            guard let path = l.socket.map({ ($0 as NSString).expandingTildeInPath }) ?? UnixSocketTransport.defaultPath(environment: env) else {
                throw TransportError("no default hab socket on this platform")
            }
            return UnixSocketTransport(path: path)
        }
        if let s = ssh {
            #if os(macOS)
            return SSHTransport(host: s.host, remoteCommand: s.command ?? [], options: s.options ?? [])
            #else
            throw TransportError("ssh to \(s.host) needs the in-app SSH carrier, which comes later")
            #endif
        }
        throw TransportError("hab entry has no endpoint")
    }
}
