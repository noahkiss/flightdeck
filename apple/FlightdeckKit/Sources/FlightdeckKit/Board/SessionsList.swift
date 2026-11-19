import Foundation
import HabAPI

/// A live or recently ended group as one row of the Sessions list (app.md 3.1, 3.8).
public struct GroupSummary: Identifiable, Hashable, Sendable {
    /// The group card's id.
    public var id: String
    public var title: String
    public var habLabel: String
    public var members: Int
    public var working: Int
    public var waiting: Int
    /// Ended or dropped: the row shows in Done.
    public var over: Bool
    public var since: Date?
    /// The member sessions the row folds in.
    public var memberTargets: [SessionTarget]

    public init(id: String, title: String, habLabel: String, members: Int, working: Int, waiting: Int, over: Bool,
                since: Date?, memberTargets: [SessionTarget]) {
        self.id = id
        self.title = title
        self.habLabel = habLabel
        self.members = members
        self.working = working
        self.waiting = waiting
        self.over = over
        self.since = since
        self.memberTargets = memberTargets
    }

    /// "Group of 2 · 1 working, 1 waiting · workbench".
    public var line: String {
        var parts = ["Group of \(members)"]
        if over {
            parts.append("Ended")
        } else {
            parts.append("\(working) working, \(waiting) waiting")
        }
        parts.append(habLabel)
        return parts.joined(separator: " · ")
    }

    /// Whether `row` is one of this group's member sessions.
    func folds(_ row: BoardRow) -> Bool {
        guard let s = row.target.session else { return false }
        return memberTargets.contains { $0.habID == row.target.habID && $0.session == s }
    }

    /// The row for one group card. `target` finds a member's session (`GroupCenter.target`).
    public static func make(_ card: GroupCard, target: (GroupMember) -> SessionTarget?) -> GroupSummary {
        let g = card.group
        var working = 0, waiting = 0
        for m in g.members {
            switch GroupText.memberState(m) {
            case "working": working += 1
            case "failed", "exited", "closed", "done", "ended", "dropped": break
            default: waiting += 1
            }
        }
        return GroupSummary(id: card.id, title: g.title, habLabel: card.habLabel, members: g.members.count,
                            working: working, waiting: waiting, over: card.isOver,
                            since: GroupCenter.parse(g.created), memberTargets: g.members.compactMap(target))
    }
}

/// One row of the Sessions list: each kind carries its own action (app.md 3.1).
public enum SessionItem: Identifiable, Equatable {
    /// A permission request: the command, Deny and Allow.
    case permission(BoardRow)
    /// A question: Answer opens the chat.
    case question(BoardRow)
    /// A confirmation card as one compact row with Review.
    case confirm(ConfirmCard)
    /// A group as one row that opens the group.
    case group(GroupSummary)
    /// Any other session row.
    case session(BoardRow)

    public var id: String {
        switch self {
        case .permission(let r), .question(let r), .session(let r): return r.id
        case .confirm(let c): return "confirm/" + c.id
        case .group(let g): return g.id
        }
    }
}

/// One section of the Sessions list.
public struct SessionsSection: Identifiable, Equatable {
    public var kind: BoardSectionKind
    public var items: [SessionItem]
    public var id: String { kind.rawValue }

    /// "Needs you · 3".
    public var title: String { "\(kind.rawValue) · \(items.count)" }
}

/// Builds the Sessions list from the board (app.md 3.1, 3.8): the board's sections, the
/// confirmation cards in Needs you, and each group as one row whose members fold into it.
public enum SessionsList {
    public static func build(sections: [BoardSection], confirms: [ConfirmCard], groups: [GroupSummary]) -> [SessionsSection] {
        var out: [BoardSectionKind: [SessionItem]] = [:]
        for section in sections {
            for row in section.rows {
                // A member that needs you stays in Needs you; the rest fold into the group's row.
                if section.kind != .needsYou, groups.contains(where: { $0.folds(row) }) { continue }
                out[section.kind, default: []].append(item(row))
            }
        }
        if !confirms.isEmpty {
            out[.needsYou, default: []] += confirms.map { .confirm($0) }
        }
        for g in groups {
            out[g.over ? .done : .working, default: []].append(.group(g))
        }
        return BoardSectionKind.allCases.compactMap { k in
            guard let items = out[k], !items.isEmpty else { return nil }
            return SessionsSection(kind: k, items: items)
        }
    }

    /// The sections without `row`, unless it needs you; an emptied section goes.
    public static func without(_ row: BoardRow, in sections: [BoardSection]) -> [BoardSection] {
        sections.compactMap { s in
            guard s.kind != .needsYou else { return s }
            var s = s
            s.rows.removeAll { $0.id == row.id }
            return s.rows.isEmpty ? nil : s
        }
    }

    /// The item for one board row: a waiting permission or question gets its own kind.
    public static func item(_ row: BoardRow) -> SessionItem {
        guard row.state == .blocked, row.rank == .blocked else { return .session(row) }
        switch row.blockedKind {
        case .question?: return .question(row)
        case .permission?, nil: return .permission(row)
        default: return .session(row)
        }
    }
}

extension BoardRow {
    /// The row's state in plain words (app.md 3.1), never the raw state name.
    public var stateWord: String {
        switch state {
        case .blocked?:
            switch blockedKind {
            case .question?: return "Asks a question"
            case .auth?: return "Needs sign-in"
            case .trust?: return "Asks to trust a folder"
            default: return "Needs permission"
            }
        case .error?: return "Stopped with an error"
        case .done?: return "Done"
        case .working?: return "Working"
        case .idle?: return "Idle"
        case .exited?: return "Exited"
        default: return target.session == nil ? "Unfiled" : "No agent"
        }
    }
}

/// First run (app.md 3.9): when the app opens on pairing, and when it asks for push.
public enum FirstRun {
    /// Opens on "Pair your computer": no hab in the app and not in demo mode.
    public static func shows(habs: Int, demo: Bool) -> Bool {
        habs == 0 && !demo
    }

    /// Asks for push permission once a hab is online, never in demo mode, and once per run.
    public static func asksForPush(online: Int, demo: Bool, asked: Bool) -> Bool {
        online > 0 && !demo && !asked
    }
}
