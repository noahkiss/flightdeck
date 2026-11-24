import Foundation

/// Next Needing You (app.md 3.16): the sessions in the Needs you section on online habs, in
/// the board's order, and the one after the open session.
public enum NextNeedingYou {
    /// The rows it moves through.
    public static func candidates(_ sections: [BoardSection]) -> [BoardRow] {
        sections.first { $0.kind == .needsYou }?.rows.filter { !$0.offline } ?? []
    }

    /// The row after `current` (by row id), wrapping round; the first with none open. Nil when
    /// no other row needs the owner.
    public static func next(in sections: [BoardSection], after current: String?) -> BoardRow? {
        let rows = candidates(sections)
        guard let current, let i = rows.firstIndex(where: { $0.id == current }) else { return rows.first }
        let n = rows.count
        guard n > 1 else { return nil }
        return rows[(i + 1) % n]
    }

    /// How many rows other than `current` need the owner.
    public static func count(in sections: [BoardSection], excluding current: String?) -> Int {
        candidates(sections).filter { $0.id != current }.count
    }
}
