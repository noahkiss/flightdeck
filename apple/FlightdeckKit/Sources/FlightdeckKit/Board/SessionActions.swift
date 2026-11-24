import Foundation
import HabAPI
import Observation

/// One action on a session row (app.md 3.11), in the menu's order.
public enum SessionAction: String, CaseIterable, Sendable {
    case rename, restart, copyName, close
}

/// What Close does to a row (app.md 3.11).
public enum SessionCloseKind: Equatable, Sendable {
    /// A running session: `pane.close` ends its program; the record stays, exited.
    case end
    /// An exited session: `session.close` removes the record.
    case remove
    /// An unfiled pane: `pane.close` ends its program.
    case endPane
}

/// A failed action, for an alert: "Couldn't rename docs" and the hab's words.
public struct SessionActionFailure: Error, Identifiable, Equatable, Sendable {
    public var id = UUID()
    public var title: String
    public var message: String?
    /// The call timed out: the hab may still have done it.
    public var timedOut = false

    public init(title: String, message: String? = nil) {
        self.title = title
        self.message = message
    }

    public static func == (a: Self, b: Self) -> Bool { a.title == b.title && a.message == b.message }
}

/// A removal or a restart, for every window to follow (app.md 3.11).
public struct SessionActionEvent: Equatable, Sendable {
    public var seq: Int
    public var rowID: String
    public var target: SessionTarget
}

/// Close, rename, restart and copy name from a row (app.md 3.11): which rows offer what, the
/// calls, and the outcomes every window follows. The questions belong to each window
/// (`SessionAsk`).
@MainActor
@Observable
public final class SessionActionCenter {
    /// The welcome capability that says a hab serves `session.restart`.
    public nonisolated static let restartCapability = "session-restart"
    /// How long a restart may take: the hab allows 60 s for the exit and the new start.
    public nonisolated static let restartTimeout: Duration = .seconds(75)

    /// The last removal: an open session view of that row closes, in every window.
    public private(set) var removal: SessionActionEvent?
    /// The last restart: an open session view of it follows to the new pane, in every window.
    /// A restart whose call timed out names no pane: windows follow the session to whatever
    /// pane it gets.
    public private(set) var restart: SessionActionEvent?
    /// Rows with a call running; their actions are off meanwhile.
    public private(set) var running: Set<String> = []
    @ObservationIgnored private var seq = 0

    @ObservationIgnored private let board: Board
    @ObservationIgnored private let connection: @MainActor (String) -> HabConnection?

    public init(board: Board, connection: @escaping @MainActor (String) -> HabConnection?) {
        self.board = board
        self.connection = connection
    }

    // MARK: - Rules

    /// What a row offers, in the menu's order.
    public func offers(_ row: BoardRow) -> [SessionAction] {
        let copy: [SessionAction] = row.name == nil ? [] : [.copyName]
        if row.offline { return copy }
        guard row.target.session != nil else { return row.target.pane == nil ? [] : [.close] }
        if row.ownedElsewhere { return copy }
        var out: [SessionAction] = [.rename]
        if canRestart(row) { out.append(.restart) }
        return out + copy + [.close]
    }

    /// Restart shows while the hab serves it, the session runs, and the agent is idle or done.
    func canRestart(_ row: BoardRow) -> Bool {
        guard !row.sessionExited, row.state == .idle || row.state == .done else { return false }
        let caps = board.habs.first { $0.id == row.target.habID }?.state.welcome?.capabilities ?? []
        return caps.contains(Self.restartCapability)
    }

    /// What Close does to the row as it is now.
    public static func closeKind(_ row: BoardRow) -> SessionCloseKind {
        if row.target.session == nil { return .endPane }
        return row.sessionExited || row.target.pane == nil ? .remove : .end
    }

    /// The row's name in questions and alerts.
    public static func name(_ row: BoardRow) -> String { row.name ?? row.title }

    /// The Close question's title, message and button.
    public static func question(_ row: BoardRow) -> (title: String, message: String, button: String) {
        switch closeKind(row) {
        case .end:
            return ("End \(name(row)) on \(row.host)?", "The agent stops. Its conversation stays, so it can be resumed.", "End session")
        case .remove:
            return ("Remove \(name(row)) from \(row.host)?", "It leaves the list. Its transcript stays on the computer.", "Remove")
        case .endPane:
            return ("End this pane's program on \(row.host)?", "The program gets a hang-up.", "End")
        }
    }

    // MARK: - Calls

    /// Close, after the question's yes. `asked` is the row as the question showed it. The row
    /// is read again first: when it changed kind meanwhile (the program ended, or an exited
    /// session runs again), nothing is sent and the answer says so. Nil when the call went out.
    public func close(_ asked: BoardRow) async -> SessionActionFailure? {
        let kind = Self.closeKind(asked)
        guard let row = board.rows.first(where: { $0.id == asked.id }) else {
            // Gone already: another client removed it.
            return kind == .remove ? nil : SessionActionFailure(title: "\(Self.name(asked)) has ended.")
        }
        guard Self.closeKind(row) == kind else {
            if kind == .remove {
                return SessionActionFailure(title: "\(Self.name(asked)) is running again.", message: "Nothing was removed.")
            }
            return SessionActionFailure(title: "\(Self.name(asked)) has ended.")
        }
        let r: Result<Void, SessionActionFailure> = await run(row, verb: kind == .remove ? "remove" : "end") { conn in
            switch kind {
            case .remove:
                _ = try await conn.call(HabMethods.sessionClose, SessionRef(session: row.target.session ?? ""))
            case .end, .endPane:
                _ = try await conn.call(HabMethods.paneClose, PaneRef(pane: row.target.pane ?? ""))
            }
        }
        if case .failure(let f) = r { return f }
        if kind == .remove { removal = event(row.id, row.target) }
        return nil
    }

    /// Rename, from the alert. A blank or unchanged name sends nothing.
    public func rename(_ row: BoardRow, to name: String) async -> SessionActionFailure? {
        let n = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !n.isEmpty, n != row.name, let s = row.target.session else { return nil }
        let r: Result<Void, SessionActionFailure> = await run(row, verb: "rename") { conn in
            _ = try await conn.call(HabMethods.sessionRename, SessionRenameParams(name: n, session: s))
        }
        if case .failure(let f) = r { return f }
        return nil
    }

    /// Restart: the hab exits the harness and starts it again in a new pane. A call that times
    /// out may still succeed on the hab, so windows then follow the session by its id.
    public func restart(_ row: BoardRow) async -> SessionActionFailure? {
        guard let s = row.target.session else { return nil }
        let r = await run(row, verb: "restart") { conn in
            try await conn.call(HabMethods.sessionRestart, SessionRef(session: s), timeout: Self.restartTimeout)
        }
        switch r {
        case .success(let made):
            restart = event(row.id, SessionTarget(habID: row.target.habID, session: made.session, pane: made.pane))
            return nil
        case .failure(let f):
            if f.timedOut { restart = event(row.id, SessionTarget(habID: row.target.habID, session: s, pane: nil)) }
            return f
        }
    }

    private func event(_ rowID: String, _ t: SessionTarget) -> SessionActionEvent {
        seq += 1
        return SessionActionEvent(seq: seq, rowID: rowID, target: t)
    }

    /// Runs one call while the row is marked running.
    private func run<T>(_ row: BoardRow, verb: String, _ call: (HabConnection) async throws -> T) async -> Result<T, SessionActionFailure> {
        let title = "Couldn't \(verb) \(Self.name(row))"
        guard let conn = connection(row.target.habID) else {
            return .failure(SessionActionFailure(title: title, message: "That computer is no longer in the app."))
        }
        running.insert(row.id)
        defer { running.remove(row.id) }
        do {
            return .success(try await call(conn))
        } catch {
            var f = SessionActionFailure(title: title, message: Self.sentence(error))
            f.timedOut = error is CallTimedOut
            return .failure(f)
        }
    }

    /// The hab's refusal as a sentence. A record error carries its kind in front
    /// ("conflict: a session named ..."); the alert leaves it off.
    static func sentence(_ error: Error) -> String {
        var t = RoundCenter.text(error)
        for k in ["conflict: ", "not_found: ", "invalid: "] where t.hasPrefix(k) {
            t.removeFirst(k.count)
        }
        return RoundCenter.sentence(t)
    }
}

/// One window's questions and alerts for the row actions (app.md 3.11). Each scene holds its
/// own, so a question shows only in the window that asked it.
@MainActor
@Observable
public final class SessionAsk {
    /// The row the Close question asks about. It stays set while the question dismisses, so
    /// the question's words do not blank; `closingShown` says whether it is up.
    public private(set) var closing: BoardRow?
    public var closingShown = false
    /// The row whose Rename alert is up, and the name in its field.
    public var renaming: BoardRow?
    public var newName = ""
    /// The last failure, or a notice ("docs has ended."), for an alert.
    public var failure: SessionActionFailure?

    public init() {}

    public func askClose(_ row: BoardRow) {
        closing = row
        closingShown = true
    }

    public func askRename(_ row: BoardRow) {
        newName = row.name ?? ""
        renaming = row
    }

    /// Runs an action, then shows its failure once the question that started it has gone.
    public func run(_ action: () async -> SessionActionFailure?) async {
        closingShown = false
        renaming = nil
        let f = await action()
        // A dialog that is still dismissing drops an alert presented in the same turn.
        await Task.yield()
        if let f { failure = f }
    }
}
