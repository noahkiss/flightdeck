import Foundation

/// The needs-you menu in the Mac menu bar (app.md 3.17): the sessions Next Needing You moves
/// through, in the board's order, and the confirmations that wait, so its count is the Dock
/// badge's.
public enum MenuBarNeeds {
    /// One session in the menu.
    public struct Item: Identifiable, Equatable, Sendable {
        public var row: BoardRow
        public var id: String { row.id }
        /// The session's title.
        public var title: String { row.title }
        /// "Needs permission · bench": what it waits for, and its hab.
        public var detail: String { row.stateWord + " · " + row.host }
        /// The mark beside it.
        public var symbol: String {
            switch row.rank {
            case .blocked: return row.blockedKind == .question ? "questionmark.bubble" : "hand.raised"
            case .error: return "exclamationmark.triangle"
            default: return "checkmark.circle"
            }
        }
    }

    /// What the menu shows.
    public enum Content: Equatable, Sendable {
        /// No hab is paired.
        case noHabs
        /// No hab is online: the Mac holds no connection while every window is closed (12.1).
        case notConnected
        /// The sessions that need the owner, and how many confirmations wait.
        case needs([Item], confirmations: Int)
    }

    @MainActor
    public static func content(_ board: Board) -> Content {
        if board.habs.isEmpty { return .noHabs }
        if board.onlineCount == 0 { return .notConnected }
        return .needs(NextNeedingYou.candidates(board.sections).map(Item.init), confirmations: board.waitingConfirmations)
    }

    /// The count beside the menu bar icon: the sessions listed plus the confirmations, as the
    /// Dock badge counts them; nil at 0 and while not connected.
    public static func count(_ content: Content) -> Int? {
        guard case .needs(let items, let c) = content, items.count + c > 0 else { return nil }
        return items.count + c
    }

    /// The confirmations line: "1 confirmation waits".
    public static func confirmationsLine(_ n: Int) -> String {
        n == 1 ? "1 confirmation waits" : "\(n) confirmations wait"
    }

    /// The menu bar icon: filled while something needs the owner.
    public static func symbol(_ content: Content) -> String {
        count(content) == nil ? "bell" : "bell.badge.fill"
    }
}
