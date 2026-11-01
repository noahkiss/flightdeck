import Foundation

/// One address a hab answers SSH on.
public struct SSHEndpoint: Codable, Sendable, Hashable, CustomStringConvertible {
    public var host: String
    public var port: Int

    public init(host: String, port: Int = 22) {
        self.host = host
        self.port = port
    }

    /// `host`, `host:port`, `[v6]` or `[v6]:port`; the port defaults to 22.
    public var description: String {
        let h = host.contains(":") ? "[\(host)]" : host
        return port == 22 ? h : "\(h):\(port)"
    }

    /// Parses `host`, `host:port`, `[v6]:port` or a bare IPv6 address.
    public static func parse(_ s: String, defaultPort: Int = 22) -> SSHEndpoint? {
        let t = s.trimmingCharacters(in: .whitespaces)
        guard !t.isEmpty, !t.contains(where: { $0.isWhitespace || $0 == "/" || $0 == "@" }) else { return nil }
        if t.hasPrefix("[") {
            guard let close = t.firstIndex(of: "]") else { return nil }
            let host = String(t[t.index(after: t.startIndex)..<close])
            let rest = t[t.index(after: close)...]
            guard !host.isEmpty else { return nil }
            if rest.isEmpty { return SSHEndpoint(host: host, port: defaultPort) }
            guard rest.hasPrefix(":"), let p = port(rest.dropFirst()) else { return nil }
            return SSHEndpoint(host: host, port: p)
        }
        let colons = t.filter { $0 == ":" }.count
        if colons == 0 { return SSHEndpoint(host: t, port: defaultPort) }
        if colons > 1 { return SSHEndpoint(host: t, port: defaultPort) } // a bare IPv6 address
        let parts = t.split(separator: ":", omittingEmptySubsequences: false)
        guard !parts[0].isEmpty, let p = port(parts[1]) else { return nil }
        return SSHEndpoint(host: String(parts[0]), port: p)
    }

    private static func port(_ s: Substring) -> Int? {
        guard let p = Int(s), (1...65535).contains(p) else { return nil }
        return p
    }
}

/// `user@host[:port]`, as typed into "Add a hab".
public struct SSHDestination: Sendable, Equatable {
    public var user: String
    public var endpoint: SSHEndpoint

    public init(user: String, endpoint: SSHEndpoint) {
        self.user = user
        self.endpoint = endpoint
    }

    public static func parse(_ s: String) -> SSHDestination? {
        let t = s.trimmingCharacters(in: .whitespaces)
        guard let at = t.lastIndex(of: "@") else { return nil }
        let user = String(t[..<at])
        guard !user.isEmpty, !user.contains(where: { $0.isWhitespace || $0 == "@" }), !user.hasPrefix("-") else { return nil }
        guard let ep = SSHEndpoint.parse(String(t[t.index(after: at)...])) else { return nil }
        return SSHDestination(user: user, endpoint: ep)
    }

    public var description: String { "\(user)@\(endpoint)" }
}
