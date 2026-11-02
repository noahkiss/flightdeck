import FlightdeckShared
import Foundation
import HabAPI
import Observation

/// One round on the status board's Rounds list (rounds.md 10.3).
public struct RoundRow: Identifiable, Hashable, Sendable {
    public var habID: String
    public var round: Round
    public var id: String { "\(habID)/r/\(round.id)" }

    public enum Mark: String, Sendable { case paused = "Paused", completed = "Completed" }

    /// Paused or completed; a completed round shows completed even when paused too.
    public var mark: Mark? {
        if round.completed == true { return .completed }
        if round.paused == true { return .paused }
        return nil
    }

    /// The run the round has live, if any.
    public var liveRun: RoundRun? { round.lastRun.flatMap { $0.state.isFinal ? nil : $0 } }
}

/// One hab's Rounds list.
public struct RoundHab: Identifiable, Hashable, Sendable {
    public var id: String
    public var label: String
    public var online: Bool
    public var offlineSince: Date?
    public var rows: [RoundRow]
}

/// The rounds of every hab in the app (rounds.md 10, app.md 21): the Rounds list per hab, the
/// actions, the round badge's names, the run session to open, and the live run panes the
/// terminal filters input for.
@MainActor
@Observable
public final class RoundCenter {
    /// The welcome capability that says a hab runs rounds.
    public static let capability = "rounds"

    /// Run sessions show on the board; off hides them (rounds.md 10.3). Kept in the defaults.
    public var showRunSessions: Bool {
        didSet { defaults.set(showRunSessions, forKey: Self.showKey) }
    }
    /// The session the board should open: a run started with Run now, or a notification's.
    public var openRequest: SessionTarget?
    /// The last refusal or failure of an action, by row id.
    public private(set) var errors: [String: String] = [:]
    /// A Delete waiting for the owner's yes.
    public var deleteAsk: RoundRow?
    /// Actions in flight, by row id.
    public private(set) var busy: Set<String> = []

    @ObservationIgnored private let board: Board
    @ObservationIgnored private let connection: @MainActor (String) -> HabConnection?
    @ObservationIgnored private let alwaysOn: @MainActor () -> Set<String>
    @ObservationIgnored private let defaults: UserDefaults
    /// A run started here whose session opens once it exists: hab id and run id.
    @ObservationIgnored private var follow: (habID: String, run: String)?
    /// A notification's session, opened once its hab shows the row: host id and session id.
    @ObservationIgnored private var pendingOpen: (host: String, session: String)?

    static let showKey = "FlightdeckShowRunSessions"

    public init(board: Board, defaults: UserDefaults = .standard,
                connection: @escaping @MainActor (String) -> HabConnection?,
                alwaysOn: @escaping @MainActor () -> Set<String> = { [] }) {
        self.board = board
        self.defaults = defaults
        self.connection = connection
        self.alwaysOn = alwaysOn
        showRunSessions = defaults.object(forKey: Self.showKey) as? Bool ?? true
    }

    // MARK: - Lists

    /// Each hab whose welcome lists `rounds`, in the app's order. An offline hab keeps the
    /// rounds it last showed.
    public var habs: [RoundHab] {
        board.habs.compactMap { h in
            guard h.state.welcome?.capabilities.contains(Self.capability) == true else { return nil }
            let rows = Self.order(Array(h.state.rounds.values)).map { RoundRow(habID: h.id, round: $0) }
            return RoundHab(id: h.id, label: h.label, online: h.state.status == .online,
                            offlineSince: h.state.status == .offline ? h.state.lostAt : nil, rows: rows)
        }
    }

    /// Whether any hab in the app runs rounds.
    public var any: Bool { board.habs.contains { $0.state.welcome?.capabilities.contains(Self.capability) == true } }

    /// The list order: rounds that will run, soonest next slot first; then rounds run only by
    /// hand; then paused; then completed. Name breaks ties.
    public static func order(_ rounds: [Round]) -> [Round] {
        func group(_ r: Round) -> Int {
            if r.completed == true { return 3 }
            if r.paused == true { return 2 }
            return r.nextSlot == nil ? 1 : 0
        }
        return rounds.sorted { a, b in
            let ga = group(a), gb = group(b)
            if ga != gb { return ga < gb }
            if ga == 0, let na = a.nextSlot.flatMap(RoundText.parse), let nb = b.nextSlot.flatMap(RoundText.parse), na != nb {
                return na < nb
            }
            if a.name != b.name { return a.name < b.name }
            return a.id < b.id
        }
    }

    public func row(habID: String, round: String) -> RoundRow? {
        board.habs.first { $0.id == habID }?.state.rounds[round].map { RoundRow(habID: habID, round: $0) }
    }

    public func store(habID: String) -> HabStore? { board.habs.first { $0.id == habID }?.state.store }

    /// The runs of one round seen in `round.run_updated` since the hab's last snapshot.
    public func habRuns(habID: String, round: String) -> [RoundRun] {
        guard let h = board.habs.first(where: { $0.id == habID }) else { return [] }
        return h.state.roundRuns.values.filter { $0.round == round }.sorted { $0.number > $1.number }
    }

    /// The round a run session belongs to, for its badge: the round's name, else "Round".
    public func badge(habID: String, session: String) -> String? {
        guard let h = board.habs.first(where: { $0.id == habID }), let r = h.state.store.sessions[session]?.round else { return nil }
        return h.state.rounds[r.round]?.name ?? "Round"
    }

    /// Whether a pane belongs to a run session whose run is not known to have ended. The
    /// terminal then drops focus reports, mouse motion and the wheel (app.md 21.4).
    public func liveRun(habID: String, pane: String) -> Bool {
        guard let h = board.habs.first(where: { $0.id == habID }),
              let s = h.state.store.sessions.values.first(where: { $0.pane == pane }), let ref = s.round else { return false }
        if let run = h.state.roundRuns[ref.run] ?? h.state.rounds[ref.round]?.lastRun.flatMap({ $0.id == ref.run ? $0 : nil }) {
            return !run.state.isFinal
        }
        // Not seen: an older run, or a run of a round that is gone. A session that exited ran out.
        return s.state != .exited
    }

    /// The board's sections with run sessions left out when `showRunSessions` is off. A run
    /// session that needs the owner always shows (app.md 21.2).
    public static func filter(_ sections: [BoardSection], showRunSessions: Bool) -> [BoardSection] {
        guard !showRunSessions else { return sections }
        return sections.compactMap { s in
            guard s.kind != .needsYou else { return s }
            let rows = s.rows.filter { $0.round == nil }
            return rows.isEmpty ? nil : BoardSection(kind: s.kind, rows: rows)
        }
    }

    // MARK: - Actions

    /// Run now: `round.run`. On success the run's session opens once it exists; a refusal
    /// (`conflict` while a run is live) shows under the round.
    public func runNow(_ row: RoundRow) async {
        await act(row) { conn in
            let r = try await conn.call(HabMethods.roundRun, RoundRef(id: row.round.id))
            self.follow = (row.habID, r.run.id)
            self.boardChanged()
        }
    }

    public func setPaused(_ row: RoundRow, _ paused: Bool) async {
        await act(row) { conn in
            _ = try await conn.call(paused ? HabMethods.roundPause : HabMethods.roundUnpause, RoundRef(id: row.round.id))
        }
    }

    /// Delete asks first: this only holds the question.
    public func askDelete(_ row: RoundRow) { deleteAsk = row }

    /// The owner said yes to the held Delete.
    public func confirmDelete() async {
        guard let row = deleteAsk else { return }
        deleteAsk = nil
        await act(row) { conn in
            _ = try await conn.call(HabMethods.roundDelete, RoundRef(id: row.round.id))
        }
    }

    /// Creates a round on a hab. Returns the round, or throws with the hab's text. With
    /// `runNow`, the run's session opens once it exists.
    @discardableResult
    public func create(habID: String, _ spec: RoundSpec, runNow: Bool) async throws -> Round {
        guard let conn = connection(habID) else { throw RoundActionError("that hab is no longer in the app") }
        do {
            let r = try await conn.call(HabMethods.roundCreate, RoundCreateParams(round: spec, runNow: runNow ? true : nil))
            if let run = r.run {
                follow = (habID, run.id)
                boardChanged()
            }
            return r.round
        } catch {
            throw RoundActionError(Self.text(error))
        }
    }

    /// Saves an edit with the revision it was made from; a stale one is refused by the hab.
    @discardableResult
    public func update(habID: String, id: String, revision: UInt64, _ spec: RoundSpec) async throws -> Round {
        guard let conn = connection(habID) else { throw RoundActionError("that hab is no longer in the app") }
        do {
            return try await conn.call(HabMethods.roundUpdate, RoundUpdateParams(id: id, revision: revision, round: spec)).round
        } catch {
            throw RoundActionError(Self.text(error))
        }
    }

    /// A page of a round's runs, newest first.
    public func runs(habID: String, round: String, before: Int? = nil, limit: Int = 50) async throws -> RoundRuns {
        guard let conn = connection(habID) else { throw RoundActionError("that hab is no longer in the app") }
        do {
            return try await conn.call(HabMethods.roundRuns, RoundRunsParams(before: before, id: round, limit: limit))
        } catch {
            throw RoundActionError(Self.text(error))
        }
    }

    public func clearError(_ row: RoundRow) { errors[row.id] = nil }

    private func act(_ row: RoundRow, _ work: @MainActor (HabConnection) async throws -> Void) async {
        errors[row.id] = nil
        guard let conn = connection(row.habID) else {
            errors[row.id] = "That hab is no longer in the app."
            return
        }
        busy.insert(row.id)
        defer { busy.remove(row.id) }
        do {
            try await work(conn)
        } catch {
            errors[row.id] = Self.sentence(Self.text(error))
        }
    }

    static func text(_ error: Error) -> String {
        if let e = error as? RPCError { return e.data?.hint ?? e.message }
        if let e = error as? RoundActionError { return e.description }
        return String(describing: error)
    }

    /// The hab's hint as a sentence: capital first letter, a full stop.
    static func sentence(_ s: String) -> String {
        guard let f = s.first else { return s }
        let t = f.uppercased() + s.dropFirst()
        return t.hasSuffix(".") ? t : t + "."
    }

    // MARK: - Opening run sessions

    /// A notification tap that names a session and no request: open that session once its
    /// hab shows it (rounds.md 10.2: a round banner's Open names the run session).
    public func open(_ meta: PushMeta) {
        guard meta.request == nil, let s = meta.session, !s.isEmpty else { return }
        pendingOpen = (meta.hab, s)
        boardChanged()
    }

    /// The board changed: open a followed run's session or a notification's session once the
    /// board has its row.
    public func boardChanged() {
        if let f = follow, let h = board.habs.first(where: { $0.id == f.habID }) {
            let run = h.state.roundRuns[f.run] ?? h.state.rounds.values.compactMap(\.lastRun).first { $0.id == f.run }
            if let s = run?.session, let row = board.rows.first(where: { $0.target.habID == f.habID && $0.target.session == s }) {
                follow = nil
                openRequest = row.target
            } else if let run, run.state.isFinal, run.session == nil {
                follow = nil // skipped before it had a session: nothing to open
            }
        }
        if let p = pendingOpen {
            for h in board.habs where (h.state.welcome?.host.id ?? h.state.store.host) == p.host {
                if let row = board.rows.first(where: { $0.target.habID == h.id && $0.target.session == p.session }) {
                    pendingOpen = nil
                    openRequest = row.target
                    break
                }
            }
        }
    }

    // MARK: - Always on (rounds.md 9)

    /// A hab marked always on that has the project, when `habID` is not: the one-line
    /// suggestion at creation. Nil when `habID` is always on or no other hab fits.
    public func suggestion(habID: String, project: String?) -> RoundHab? {
        let on = alwaysOn()
        guard !on.contains(habID), let h = board.habs.first(where: { $0.id == habID }) else { return nil }
        let p = project.flatMap { h.state.store.projects[$0] }
        for other in board.habs where other.id != habID && on.contains(other.id) && other.state.status == .online
            && other.state.welcome?.capabilities.contains(Self.capability) == true {
            if let p, Self.project(like: p, in: other.state.store) == nil { continue }
            return RoundHab(id: other.id, label: other.label, online: true, rows: [])
        }
        return nil
    }

    /// The project on another hab that is `p`: the same remote, else the same name when
    /// neither has a remote.
    public static func project(like p: Project, in store: HabStore) -> Project? {
        if let r = p.remote, !r.isEmpty { return store.projects.values.first { $0.remote == r } }
        return store.projects.values.first { ($0.remote ?? "").isEmpty && $0.name == p.name }
    }
}

/// The round page's run list: a page from `round.runs` with the run updates seen since.
public enum RoundPageRuns {
    /// `page` with each run replaced by its update, and runs newer than the page's newest
    /// added on top; newest first.
    public static func merge(_ page: [RoundRun], _ updates: [RoundRun]) -> [RoundRun] {
        var byID = Dictionary(page.map { ($0.id, $0) }, uniquingKeysWith: { a, _ in a })
        let top = page.map(\.number).max() ?? 0
        for u in updates where byID[u.id] != nil || u.number > top { byID[u.id] = u }
        return byID.values.sorted { $0.number > $1.number }
    }
}

public struct RoundActionError: Error, CustomStringConvertible, Sendable {
    public var description: String
    init(_ d: String) { description = d }
}
