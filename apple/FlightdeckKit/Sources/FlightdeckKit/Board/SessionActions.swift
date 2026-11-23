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
public struct SessionActionFailure: Identifiable, Equatable, Sendable {
    public var id = UUID()
    public var title: String
    public var message: String?

    public static func == (a: Self, b: Self) -> Bool { a.title == b.title && a.message == b.message }
}

/// Close, rename, restart and copy name from a row (app.md 3.11): which rows offer what, the
/// questions, the calls, and the outcome the shell follows.
@MainActor
@Observable
public final class SessionActionCenter {
    /// The welcome capability that says a hab serves `session.restart`.
    public nonisolated static let restartCapability = "session-restart"

    /// The row whose Close question is up.
    public var closing: BoardRow?
    /// The row whose Rename alert is up, and the name in its field.
    public var renaming: BoardRow?
    public var newName = ""
    /// The last failure, or a notice ("docs has ended."), for an alert.
    public var failure: SessionActionFailure?
    /// A removed session's row id: an open session view of it closes.
    public private(set) var removed: String?
    /// A restarted session's new target: an open session view of it follows.
    public var restarted: SessionTarget?
    /// Rows with a call running.
    public private(set) var running: Set<String> = []

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

    // MARK: - Starting an action

    public func askClose(_ row: BoardRow) { closing = row }

    public func askRename(_ row: BoardRow) {
        newName = row.name ?? ""
        renaming = row
    }

    // MARK: - Calls

    /// Close, after the question's yes. `asked` is the row as the question showed it. Before
    /// an End goes out the row is read again: a program that ended meanwhile gets no call.
    public func close(_ asked: BoardRow) async {
        closing = nil
        let kind = Self.closeKind(asked)
        let now = board.rows.first { $0.id == asked.id }
        if kind == .end, now == nil || now.map(Self.closeKind) != .end {
            failure = SessionActionFailure(title: "\(Self.name(asked)) has ended.")
            return
        }
        let row = now ?? asked
        let done: Void? = await run(row, verb: kind == .remove ? "remove" : "end") { conn in
            switch kind {
            case .remove:
                _ = try await conn.call(HabMethods.sessionClose, SessionRef(session: row.target.session ?? ""))
            case .end, .endPane:
                _ = try await conn.call(HabMethods.paneClose, PaneRef(pane: row.target.pane ?? ""))
            }
        }
        if kind == .remove, done != nil { removed = row.id }
    }

    /// Rename, from the alert. A blank or unchanged name sends nothing.
    public func rename(_ row: BoardRow, to name: String) async {
        renaming = nil
        let n = name.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !n.isEmpty, n != row.name, let s = row.target.session else { return }
        _ = await run(row, verb: "rename") { conn in
            _ = try await conn.call(HabMethods.sessionRename, SessionRenameParams(name: n, session: s))
        }
    }

    /// Restart: the hab exits the harness and starts it again in a new pane.
    public func restart(_ row: BoardRow) async {
        guard let s = row.target.session else { return }
        let made = await run(row, verb: "restart") { conn in
            try await conn.call(HabMethods.sessionRestart, SessionRef(session: s))
        }
        if let made {
            restarted = SessionTarget(habID: row.target.habID, session: made.session, pane: made.pane)
        }
    }

    /// The shell followed a removal.
    public func removalSeen() { removed = nil }

    /// Runs one call; nil when it failed, with the failure set for the alert.
    private func run<T>(_ row: BoardRow, verb: String, _ call: (HabConnection) async throws -> T) async -> T? {
        guard let conn = connection(row.target.habID) else {
            failure = SessionActionFailure(title: "Couldn't \(verb) \(Self.name(row))", message: "That computer is no longer in the app.")
            return nil
        }
        running.insert(row.id)
        defer { running.remove(row.id) }
        do {
            return try await call(conn)
        } catch {
            failure = SessionActionFailure(title: "Couldn't \(verb) \(Self.name(row))", message: Self.sentence(error))
            return nil
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

extension SessionActionFailure {
    init(title: String) { self.init(title: title, message: nil) }
}
