import Foundation

/// What the needs-you widget shows (app.md 15.4), written by the app to the App Group: the
/// sessions that wait for the owner, by ids, session name and hab label. It carries no command
/// text, no folder and no address: the widget sits on the Home and Lock Screens.
public struct NeedsYouSnapshot: Codable, Equatable, Sendable {
    public static let version = 1
    /// The most items the snapshot keeps; the large widget shows four.
    public static let maxItems = 4

    public struct Item: Codable, Equatable, Sendable, Identifiable {
        /// What the session waits for.
        public enum Kind: String, Codable, Sendable {
            /// A permission request with a request id: Allow and Deny can answer it.
            case approval
            case question
            /// Anything else in Needs you: another blocked kind, an error, a finished turn.
            case other
        }

        public var kind: Kind
        /// The session's name, as the board shows it.
        public var title: String
        public var habLabel: String
        /// The hab's host id.
        public var hab: String
        public var session: String?
        public var pane: String?
        /// The request id, for Allow and Deny.
        public var request: String?
        public var since: Date?

        public var id: String { [hab, session ?? "", pane ?? "", request ?? ""].joined(separator: "/") }

        public init(kind: Kind, title: String, habLabel: String, hab: String, session: String?, pane: String?, request: String?, since: Date?) {
            self.kind = kind
            self.title = title
            self.habLabel = habLabel
            self.hab = hab
            self.session = session
            self.pane = pane
            self.request = request
            // Whole seconds, as the snapshot stores it, so a stored item equals a new one.
            self.since = since.map { Date(timeIntervalSince1970: $0.timeIntervalSince1970.rounded(.down)) }
        }

        /// Where a tap goes: the approval sheet for a request, else the session.
        public var url: URL {
            if kind != .other, let pane {
                return RequestLink.url(hab: hab, pane: pane, request: request, session: session)
            }
            var c = URLComponents()
            c.scheme = "flightdeck"
            c.host = "session"
            c.queryItems = [URLQueryItem(name: "hab", value: hab), URLQueryItem(name: "session", value: session ?? "")]
            return c.url!
        }
    }

    public var version: Int
    public var writtenAt: Date
    /// Everything that needs the owner, the board's needs-you count; may exceed `items`.
    public var count: Int
    public var items: [Item]
    public init(writtenAt: Date, count: Int, items: [Item]) {
        version = Self.version
        self.writtenAt = writtenAt
        self.count = count
        self.items = Array(items.prefix(Self.maxItems))
    }

    /// The App Group defaults key.
    public static let defaultsKey = "flightdeck.needs-you.snapshot"
    /// The widget kind, for reloads.
    public static let widgetKind = "flightdeck.needs-you"

    public static func load(_ defaults: UserDefaults = SharedSettings.defaults) -> NeedsYouSnapshot? {
        guard let data = defaults.data(forKey: defaultsKey) else { return nil }
        let d = JSONDecoder()
        d.dateDecodingStrategy = .iso8601
        guard let s = try? d.decode(NeedsYouSnapshot.self, from: data), s.version == version else { return nil }
        return s
    }

    /// The snapshot without the requests a hab reported resolved (app.md 15.4): the request
    /// with that id, or, when the report names none (a question carries no id), the request
    /// items of that pane with no id. Only items of `hab` go. The count drops by what left.
    public func withdrawing(hab: String, request: String?, pane: String?) -> NeedsYouSnapshot {
        let gone: (Item) -> Bool = { it in
            guard it.hab == hab, it.kind != .other else { return false }
            if let request, !request.isEmpty { return it.request == request }
            guard let pane, !pane.isEmpty else { return false }
            return it.pane == pane && (it.request ?? "").isEmpty
        }
        var s = self
        let left = items.filter { !gone($0) }
        s.count = max(left.count, count - (items.count - left.count))
        s.items = left
        return s
    }

    /// Drops what a withdrawal push names from the stored snapshot (the push kind `answered`,
    /// hab-logic-api.md 3.9). True when the snapshot changed, so the widget should reload.
    @discardableResult
    public static func withdraw(_ meta: PushMeta, defaults: UserDefaults = SharedSettings.defaults) -> Bool {
        guard meta.kind == "answered", let s = load(defaults) else { return false }
        let next = s.withdrawing(hab: meta.hab, request: meta.request, pane: meta.pane)
        guard next.items != s.items else { return false }
        return next.save(defaults)
    }

    /// Writes the snapshot; false when what the widget shows is unchanged.
    @discardableResult
    public func save(_ defaults: UserDefaults = SharedSettings.defaults) -> Bool {
        if let old = Self.load(defaults), old.count == count, old.items == items { return false }
        let e = JSONEncoder()
        e.dateEncodingStrategy = .iso8601
        e.outputFormatting = [.sortedKeys]
        guard let data = try? e.encode(self) else { return false }
        defaults.set(data, forKey: Self.defaultsKey)
        return true
    }
}
