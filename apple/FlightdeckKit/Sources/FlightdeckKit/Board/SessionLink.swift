import Foundation

/// A session by ids only (app.md 3.14, 3.15): the hab's host id and the session id. A link, a
/// Handoff activity and a session window carry it. It names no pane, so it follows a restart.
public struct SessionLink: Codable, Hashable, Sendable {
    /// The hab's own host id (its welcome's `host.id`); the app's connection id while the
    /// app has not seen the host id.
    public var hab: String
    public var session: String

    public init(hab: String, session: String) {
        self.hab = hab
        self.session = session
    }

    // MARK: - The link

    /// `flightdeck://session?hab=<host id>&session=<session id>`.
    public var url: URL {
        var c = URLComponents()
        c.scheme = "flightdeck"
        c.host = "session"
        c.queryItems = [URLQueryItem(name: "hab", value: hab), URLQueryItem(name: "session", value: session)]
        return c.url!
    }

    /// Reads a session link; nil for any other link, or one with no hab or session.
    public init?(url: URL) {
        guard url.scheme == "flightdeck", url.host == "session",
              let items = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems,
              let hab = items.first(where: { $0.name == "hab" })?.value, !hab.isEmpty,
              let session = items.first(where: { $0.name == "session" })?.value, !session.isEmpty
        else { return nil }
        self.init(hab: hab, session: session)
    }

    // MARK: - Handoff

    /// The Handoff activity type; both apps list it in `NSUserActivityTypes`.
    public static var activityType: String { (Bundle.main.bundleIdentifier ?? "flightdeck") + ".session" }

    /// How long a session stays on screen before it advertises Handoff (app.md 3.15).
    public static let handoffDwell: Duration = .seconds(3)

    /// The activity's user info: ids only.
    public var userInfo: [String: String] { ["hab": hab, "session": session] }

    /// Reads an activity's user info back; nil when an id is missing.
    public init?(userInfo: [AnyHashable: Any]?) {
        guard let hab = userInfo?["hab"] as? String, !hab.isEmpty,
              let session = userInfo?["session"] as? String, !session.isEmpty
        else { return nil }
        self.init(hab: hab, session: session)
    }

    // MARK: - The board

    /// The id a link names a hab by: its host id, else the app's connection id.
    @MainActor
    public static func hostID(_ h: Board.Hab) -> String {
        knownHostID(h) ?? h.id
    }

    @MainActor
    static func knownHostID(_ h: Board.Hab) -> String? {
        let id = h.state.welcome?.host.id ?? h.state.store.host
        return id?.isEmpty == false ? id : nil
    }

    /// The link of a row; nil for an unfiled pane, which has no session.
    @MainActor
    public static func of(_ row: BoardRow, board: Board) -> SessionLink? {
        guard let s = row.target.session else { return nil }
        let hab = board.habs.first { $0.id == row.target.habID }.map(hostID) ?? row.target.habID
        return SessionLink(hab: hab, session: s)
    }

    /// Where a link stands on this device's board.
    public enum Resolution: Equatable, Sendable {
        /// The board lists the session: open this row.
        case row(BoardRow)
        /// A hab may still list it: wait.
        case waiting
        /// No hab in the app is the link's hab.
        case unknownHab
    }

    /// Resolves the link: the row once listed; unknown only when every hab in the app has said
    /// its host id and none is the link's.
    @MainActor
    public func resolve(in board: Board) -> Resolution {
        let habs = board.habs.filter { $0.id == hab || Self.knownHostID($0) == hab }
        if habs.isEmpty {
            return board.habs.allSatisfy { Self.knownHostID($0) != nil } ? .unknownHab : .waiting
        }
        let ids = Set(habs.map(\.id))
        if let r = board.rows.first(where: { ids.contains($0.target.habID) && $0.target.session == session }) {
            return .row(r)
        }
        return .waiting
    }
}

/// What a link from outside the app is (app.md 3.15). Only these reach a handler; anything
/// else, a malformed session link too, is ignored.
public enum AppLink: Equatable, Sendable {
    case file
    case session(SessionLink)
    case usage
    case request
    case pair
    case unknown

    public static func kind(_ url: URL) -> AppLink {
        if url.isFileURL { return .file }
        guard url.scheme == "flightdeck" else { return .unknown }
        switch url.host {
        case "session": return SessionLink(url: url).map(AppLink.session) ?? .unknown
        case "usage": return .usage
        case "request": return .request
        case "pair": return .pair
        default: return .unknown
        }
    }
}

/// A link or Handoff that waits for the board to list its session, within the restore's
/// minute (app.md 3.15).
public struct PendingLink: Equatable, Sendable {
    public var link: SessionLink
    public var since: Date

    public init(_ link: SessionLink, now: Date) {
        self.link = link
        since = now
    }

    public func expired(now: Date) -> Bool {
        now.timeIntervalSince(since) > SessionRestore.limit
    }
}
