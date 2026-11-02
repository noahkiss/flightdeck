import Foundation
import HabAPI

/// The groups of the status board, top to bottom (app.md 3.1).
public enum BoardRank: Int, Comparable, Sendable, CaseIterable {
    /// `blocked`: permission, question or auth. The agent waits for the owner.
    case blocked = 1
    /// `error`: the turn stopped.
    case error = 2
    /// `done`, not yet seen.
    case doneUnseen = 3
    /// `working`.
    case working = 4
    /// `idle`, or `done` seen.
    case atRest = 5
    /// `none`, `exited`, no agent, or an unfiled pane.
    case other = 6

    public static func < (a: BoardRank, b: BoardRank) -> Bool { a.rawValue < b.rawValue }

    public var section: BoardSectionKind {
        switch self {
        case .blocked, .error, .doneUnseen: return .needsYou
        case .working: return .working
        case .atRest: return .done
        case .other: return .other
        }
    }
}

/// The board's section labels, in plain words (app.md 3.1).
public enum BoardSectionKind: String, Sendable, CaseIterable, Identifiable {
    case needsYou = "Needs you"
    case working = "Working"
    case done = "Done"
    case other = "Other"

    public var id: String { rawValue }
}

/// What a row opens: a session on a hab, or an unfiled pane. Later views
/// (chat, terminal) take this.
public struct SessionTarget: Hashable, Sendable {
    public var habID: String
    public var session: String?
    public var pane: String?

    public init(habID: String, session: String?, pane: String?) {
        self.habID = habID
        self.session = session
        self.pane = pane
    }
}

/// One row of the status board: a session, or an unfiled pane.
public struct BoardRow: Identifiable, Hashable, Sendable {
    public var id: String
    public var target: SessionTarget
    /// The hab's label, shown on every row.
    public var host: String
    public var title: String
    /// The pending command, or the request's context line.
    public var context: String?
    public var project: String?
    public var model: String?
    public var harness: String?
    public var state: AgentStatus?
    public var blockedKind: BlockedKind?
    public var contextPct: Int?
    public var rank: BoardRank
    /// When the agent entered its state, by the hab's clock.
    public var since: Date?
    /// The hab is offline: the row is dimmed and offers no action.
    public var offline = false
    public var offlineSince: Date?
    /// The key that marks a `done` as seen: the pane and the state's stamp.
    public var seenKey: String?
    /// A run session's round (rounds.md 10.3): the round's name, "Round" when the round is
    /// gone. Nil for any other session.
    public var round: String?
}

/// Builds and ranks rows from one hab's objects.
public enum BoardRows {
    /// The rows of one hab, unordered.
    public static func build(habID: String, host: String, store: HabStore, seen: Set<String>, rounds: [String: Round] = [:]) -> [BoardRow] {
        var rows: [BoardRow] = []
        for s in store.sessions.values {
            let agent = s.pane.flatMap { store.agents[$0] }
            let key = agent.map { seenKey(habID: habID, pane: $0.pane, at: $0.at) }
            var row = BoardRow(
                id: "\(habID)/s/\(s.id)",
                target: SessionTarget(habID: habID, session: s.id, pane: s.pane),
                host: host,
                title: title(session: s, agent: agent),
                context: agent?.request.flatMap { $0.command ?? $0.contextLine },
                project: s.project.flatMap { store.projects[$0]?.name },
                model: s.model,
                harness: agent?.harness,
                state: agent?.state,
                blockedKind: agent?.kind,
                contextPct: s.contextPct,
                rank: rank(session: s, agent: agent, seen: key.map { seen.contains($0) } ?? false),
                since: agent.flatMap { parseStamp($0.at) },
                seenKey: key
            )
            if s.state == .exited && agent == nil { row.state = .exited }
            if let r = s.round { row.round = rounds[r.round]?.name ?? "Round" }
            rows.append(row)
        }
        // Unfiled panes: rank 6 under their hab, so the owner can file or close them.
        for p in store.panes.values where p.session == nil || store.sessions[p.session!] == nil {
            let agent = store.agents[p.id]
            rows.append(BoardRow(
                id: "\(habID)/p/\(p.id)",
                target: SessionTarget(habID: habID, session: nil, pane: p.id),
                host: host,
                title: "Unfiled pane \(p.id)",
                harness: agent?.harness,
                state: agent?.state,
                rank: .other,
                since: agent.flatMap { parseStamp($0.at) }
            ))
        }
        return rows
    }

    /// The rank of a session (app.md 3.1).
    public static func rank(session: Session, agent: AgentState?, seen: Bool) -> BoardRank {
        if session.state == .exited { return .other }
        guard let a = agent else { return .other }
        switch a.state {
        case .blocked: return .blocked
        case .error: return .error
        case .done: return seen ? .atRest : .doneUnseen
        case .working: return .working
        case .idle: return .atRest
        default: return .other
        }
    }

    /// The pending request's title for a permission; else the session's title
    /// or name.
    static func title(session: Session, agent: AgentState?) -> String {
        if let a = agent, a.state == .blocked, a.kind == .permission, let r = a.request {
            if let t = r.title, !t.isEmpty { return t }
            if let tool = r.tool { return "Allow \(tool)?" }
        }
        if let t = session.title, !t.isEmpty { return t }
        return session.name
    }

    static func seenKey(habID: String, pane: String, at: String) -> String {
        "\(habID)/\(pane)@\(at)"
    }

    static func parseStamp(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }
}
