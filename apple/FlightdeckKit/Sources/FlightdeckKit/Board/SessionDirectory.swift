import FlightdeckShared
import Foundation
import HabAPI
import Observation

/// One session as Shortcuts, Siri and Spotlight know it (app.md 3.19): ids and display names
/// only. No command, folder, project path or address.
public struct SessionEntry: Codable, Hashable, Sendable, Identifiable {
    /// The hab's host id.
    public var hab: String
    public var session: String
    /// The session's name, as the board shows it.
    public var name: String
    public var habLabel: String

    public init(hab: String, session: String, name: String, habLabel: String) {
        self.hab = hab
        self.session = session
        self.name = name
        self.habLabel = habLabel
    }

    /// Stable across launches and devices: the session link, which names the hab by host id.
    public var id: String { link.url.absoluteString }
    public var link: SessionLink { SessionLink(hab: hab, session: session) }
}

/// The sessions App Intents can name (app.md 3.19), kept on the device so a query answers
/// while the app is closed and its connections are down. An online hab's list replaces what
/// was kept for it; an offline hab keeps its last list; a hab no longer in the app loses its.
@MainActor
public final class SessionDirectory {
    public private(set) var entries: [SessionEntry]
    /// Told what changed, for the Spotlight index: entries to add or update, and ids to delete.
    public var onChange: (@MainActor (_ upsert: [SessionEntry], _ delete: [String]) -> Void)?

    private let defaults: UserDefaults?
    static let key = "flightdeck.intents.sessions"

    /// `defaults` nil keeps the list in memory only (tests, demo).
    public init(defaults: UserDefaults?) {
        self.defaults = defaults
        if let data = defaults?.data(forKey: Self.key), let e = try? JSONDecoder().decode([SessionEntry].self, from: data) {
            entries = e
        } else {
            entries = []
        }
    }

    /// Reads the board again. A hab whose host id is not known yet keeps what it had.
    public func update(_ board: Board) {
        var known: [String: [SessionEntry]] = Dictionary(grouping: entries, by: \.hab)
        var hosts: Set<String> = []
        var unknownHostPresent = false
        for h in board.habs {
            guard let host = SessionLink.knownHostID(h) else {
                unknownHostPresent = true
                continue
            }
            hosts.insert(host)
            guard h.state.status == .online else { continue }
            // The session's own title or name: a row's title can be a request's, which may
            // quote a command.
            known[host] = board.rows.compactMap { row -> SessionEntry? in
                guard row.target.habID == h.id, let sid = row.target.session else { return nil }
                return SessionEntry(hab: host, session: sid, name: Self.name(h.state.store.sessions[sid], fallback: row.name ?? sid), habLabel: h.label)
            }
        }
        // A hab no longer in the app: its sessions go, once every hab has said its host id.
        if !unknownHostPresent {
            for host in known.keys where !hosts.contains(host) { known[host] = nil }
        }
        let next = known.values.flatMap { $0 }.sorted { ($0.name.lowercased(), $0.id) < ($1.name.lowercased(), $1.id) }
        apply(next)
    }

    /// A session's display name: its title, else its name.
    static func name(_ s: Session?, fallback: String) -> String {
        if let t = s?.title, !t.isEmpty { return t }
        if let n = s?.name, !n.isEmpty { return n }
        return fallback
    }

    /// Forgets every session (demo data must never reach Spotlight).
    public func clear() {
        apply([])
    }

    private func apply(_ next: [SessionEntry]) {
        guard next != entries else { return }
        let old = Dictionary(uniqueKeysWithValues: entries.map { ($0.id, $0) })
        let new = Set(next.map(\.id))
        let upsert = next.filter { old[$0.id] != $0 }
        let delete = old.keys.filter { !new.contains($0) }.sorted()
        entries = next
        if let defaults, let data = try? JSONEncoder().encode(next) { defaults.set(data, forKey: Self.key) }
        onChange?(upsert, delete)
    }

    // MARK: - Queries

    /// The entries with these ids, in the order asked; unknown ids are left out.
    public func entries(for ids: [String]) -> [SessionEntry] {
        let byID = Dictionary(uniqueKeysWithValues: entries.map { ($0.id, $0) })
        return ids.compactMap { byID[$0] }
    }

    /// Sessions whose name or hab label holds `text`, ignoring case and accents; names that
    /// start with it first.
    public func matching(_ text: String) -> [SessionEntry] {
        let q = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !q.isEmpty else { return entries }
        let opts: String.CompareOptions = [.caseInsensitive, .diacriticInsensitive]
        let hits = entries.filter { $0.name.range(of: q, options: opts) != nil || $0.habLabel.range(of: q, options: opts) != nil }
        let prefix = hits.filter { $0.name.range(of: q, options: opts.union(.anchored)) != nil }
        return prefix + hits.filter { e in !prefix.contains(e) }
    }
}

/// Routes from App Intents, waiting for a window to take them (app.md 3.19).
@MainActor
@Observable
public final class AppRoutes {
    /// The route no window has taken yet.
    public private(set) var pending: AppRoute?
    /// Raised on each post, so a window sees a repeat of the same route.
    public private(set) var serial = 0
    @ObservationIgnored private var postedAt = Date.distantPast

    public init() {}

    public func post(_ route: AppRoute, now: Date = .now) {
        pending = route
        postedAt = now
        serial += 1
    }

    /// Takes the pending route when `accept` says so. A route older than the restore's minute
    /// lapses (3.15), so a window opened much later never acts on it.
    public func take(now: Date = .now, _ accept: (AppRoute) -> Bool) -> AppRoute? {
        guard let r = pending else { return nil }
        if now.timeIntervalSince(postedAt) > SessionRestore.limit {
            pending = nil
            return nil
        }
        guard accept(r) else { return nil }
        pending = nil
        return r
    }
}

extension Fleet {
    /// The board changed: the session list for App Intents and Spotlight, and the needs-you
    /// widget. Demo data reaches neither.
    func publishSessions() {
        guard !isDemo else { return }
        directory?.update(board)
        // Going to the background stops the habs one at a time: a write between two stops would
        // drop the stopped habs' items. The widget keeps what it had while the app is away.
        if isActive { needsYou?.update(board) }
    }

    /// The oldest request that waits, in one session when one is named (app.md 3.19): the
    /// sheet Approve pending opens. The hab is named by host id.
    public func waitingRequest(hab: String?, session: String?) -> OpenRequest? {
        OpenRequest.collect(board.habs).first { r in
            guard !r.offline, r.kind == .permission || r.kind == .question else { return false }
            if let hab, r.host != hab { return false }
            if let session, r.session != session { return false }
            return true
        }
    }

    /// Every hab in the app is online: the board is current, so "nothing waits" is true.
    public var boardIsCurrent: Bool {
        isActive && !board.habs.isEmpty && board.onlineCount == board.habs.count
    }
}

/// Writes the needs-you widget's snapshot (app.md 15.4) from the board, and asks the widget to
/// reload when what it shows changed.
@MainActor
public final class NeedsYouPublisher {
    private let defaults: UserDefaults
    /// Asks the widget to reload; tests count calls.
    var reload: () -> Void
    private let now: () -> Date

    public init(defaults: UserDefaults = SharedSettings.defaults, now: @escaping () -> Date = Date.init, reload: @escaping () -> Void) {
        self.defaults = defaults
        self.now = now
        self.reload = reload
    }

    /// Writes only while some hab is online: in the background every connection is down, and
    /// the widget keeps the last list the app saw rather than an empty one.
    public func update(_ board: Board) {
        guard board.onlineCount > 0 else { return }
        if Self.snapshot(board, now: now()).save(defaults) { reload() }
    }

    /// Drops an answered request from the stored snapshot, and reloads when it was there.
    public static func drop(request: String, defaults: UserDefaults = SharedSettings.defaults, reload: () -> Void) {
        guard var s = NeedsYouSnapshot.load(defaults), s.items.contains(where: { $0.request == request }) else { return }
        s.items.removeAll { $0.request == request }
        s.count = max(s.items.count, s.count - 1)
        if s.save(defaults) { reload() }
    }

    /// The snapshot of `board`: the requests that wait, oldest first, then the other rows in
    /// Needs you.
    public static func snapshot(_ board: Board, now: Date) -> NeedsYouSnapshot {
        let requests = OpenRequest.collect(board.habs).filter { !$0.offline }
        var items: [NeedsYouSnapshot.Item] = requests.compactMap { r in
            guard let host = r.host else { return nil }
            let kind: NeedsYouSnapshot.Item.Kind = r.answerable ? .approval : (r.kind == .question ? .question : .other)
            return NeedsYouSnapshot.Item(kind: kind, title: r.sessionName, habLabel: r.habLabel, hab: host,
                                         session: r.session, pane: r.pane, request: r.requestID, since: r.since)
        }
        let blocked = Set(requests.map { "\($0.habID)/\($0.pane)" })
        for row in NextNeedingYou.candidates(board.sections) where !blocked.contains("\(row.target.habID)/\(row.target.pane ?? "")") {
            guard let link = SessionLink.of(row, board: board) else { continue }
            let session = board.habs.first { $0.id == row.target.habID }?.state.store.sessions[link.session]
            items.append(NeedsYouSnapshot.Item(kind: .other, title: SessionDirectory.name(session, fallback: row.name ?? link.session),
                                               habLabel: row.host, hab: link.hab,
                                               session: link.session, pane: nil, request: nil, since: row.since))
        }
        return NeedsYouSnapshot(writtenAt: now, count: max(board.needsYouCount, items.count), items: items)
    }
}
