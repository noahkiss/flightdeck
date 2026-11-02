import Foundation
import os

/// Which hab connection answers for a host id. A push names its hab by host id
/// (the welcome's `host.id`); a notification action that runs in the
/// background has no live connection and looks the entry up here. The app
/// writes a pair each time a hab comes online.
///
/// The file is `Flightdeck/hab-book.json` under Application Support (or
/// `$FLIGHTDECK_HAB_BOOK`), mode 0600. It holds ids only, no addresses.
public final class HabBook: @unchecked Sendable {
    private let url: URL?
    private let lock = NSLock()
    private var map: [String: Entry] = [:]
    private let log = Logger(subsystem: "com.flightdeck.app", category: "push")

    public struct Entry: Codable, Equatable, Sendable {
        /// The hab connection's id in the app (`nio:<entry>` or a config id).
        public var connection: String
        /// The label the board shows, for "Could not reach <hab>".
        public var label: String
        /// The hab's device id from its welcome: a push names the Capcom host by it
        /// (capcom.md 8.5). Absent in a book from before.
        public var device: String? = nil
    }

    public init(url: URL?) {
        self.url = url
        if let url, let data = try? Data(contentsOf: url),
           let m = try? JSONDecoder().decode([String: Entry].self, from: data) {
            map = m
        }
    }

    public static func path(environment env: [String: String] = ProcessInfo.processInfo.environment) -> URL? {
        if let p = env["FLIGHTDECK_HAB_BOOK"], !p.isEmpty {
            return URL(fileURLWithPath: (p as NSString).expandingTildeInPath)
        }
        return FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first?
            .appendingPathComponent("Flightdeck/hab-book.json")
    }

    public func entry(host: String) -> Entry? {
        lock.withLock { map[host] }
    }

    /// The entry of the hab with this device id.
    public func entry(device: String) -> Entry? {
        lock.withLock { map.values.first { $0.device == device } }
    }

    /// Records a pair; writes the file only when it changed.
    public func record(host: String, connection: String, label: String, device: String? = nil) {
        let e = Entry(connection: connection, label: label, device: device)
        let snapshot: [String: Entry]? = lock.withLock {
            if map[host] == e { return nil }
            // One connection answers for one host: drop an older pair for it.
            for (k, v) in map where v.connection == connection && k != host { map[k] = nil }
            map[host] = e
            return map
        }
        if let snapshot { write(snapshot) }
    }

    /// Drops every pair of a removed connection, in memory and in the file.
    public func forget(connection: String) {
        let snapshot: [String: Entry]? = lock.withLock {
            let before = map.count
            for (k, v) in map where v.connection == connection { map[k] = nil }
            return map.count == before ? nil : map
        }
        if let snapshot { write(snapshot) }
    }

    private func write(_ snapshot: [String: Entry]) {
        guard let url else { return }
        do {
            try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            let data = try JSONEncoder().encode(snapshot)
            try data.write(to: url, options: .atomic)
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
        } catch {
            log.error("hab book not written: \(String(describing: error), privacy: .private)")
        }
    }
}
