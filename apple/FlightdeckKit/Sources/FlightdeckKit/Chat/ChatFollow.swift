import CoreGraphics
import Foundation

/// Whether a thread follows its newest row, and what the jump-to-latest button shows (app.md
/// 4.12). One value per visible thread; the session chat and the Capcom thread both use it.
///
/// "Following" is sticky. Only the owner's own scroll beyond `tolerance` clears it. Content that
/// grows under a streaming reply, a keyboard or a taller composer never does: while following, the
/// thread goes back to the bottom once no finger moves it.
public struct ChatFollow: Equatable, Sendable {
    /// How far from the bottom still counts as at the bottom, in points.
    public static let tolerance: CGFloat = 48

    /// Who moves the thread now.
    public enum Motion: Equatable, Sendable {
        /// Nothing: content or insets may change.
        case idle
        /// The owner's finger, or the fling after it.
        case user
        /// A scroll the app started.
        case animating
    }

    /// What the view does after an input.
    public enum Action: Equatable, Sendable {
        case none
        case toBottom
        /// Keep this row at the top: older rows went in above it.
        case keep(String)
    }

    public private(set) var following = true
    /// Rows that arrived below while the owner read further up.
    public private(set) var unread = 0

    public init() {}

    /// The jump-to-latest button shows while the owner reads further up.
    public var showsJump: Bool { !following }

    /// The scroll geometry changed: `distance` is how far the visible bottom is above the
    /// content's end, insets included.
    public mutating func scrolled(distance: CGFloat, motion: Motion) -> Action {
        if distance <= Self.tolerance {
            // At the bottom, or back at it.
            following = true
            unread = 0
            return .none
        }
        switch motion {
        case .user:
            following = false
            return .none
        case .idle:
            // The content grew or the bars changed height under a following thread.
            return following ? .toBottom : .none
        case .animating:
            return .none
        }
    }

    /// The rows changed: `old` and `new` are the row ids before and after, in order.
    public mutating func rowsChanged(from old: [String], to new: [String]) -> Action {
        guard let oldLast = old.last else {
            following = true
            unread = 0
            return new.isEmpty ? .none : .toBottom
        }
        guard let newLast = new.last else { return .none }
        let firstChanged = old.first != new.first
        let lastChanged = oldLast != newLast
        if firstChanged && !lastChanged, let f = old.first { return .keep(f) }
        if lastChanged, let i = new.lastIndex(of: oldLast) {
            // Rows appended after the old last row.
            if following { return .toBottom }
            unread += new.count - 1 - i
            return .none
        }
        if lastChanged {
            // A replaced conversation (a new tail, a reset): start at its end.
            following = true
            unread = 0
            return .toBottom
        }
        return .none
    }

    /// The owner sent a message, or tapped the jump button: show the newest row.
    public mutating func jump() -> Action {
        following = true
        unread = 0
        return .toBottom
    }

    /// The button's words: "3 new messages", or "Jump to latest" with none counted.
    public static func jumpLabel(_ unread: Int) -> String {
        switch unread {
        case 0: return "Jump to latest"
        case 1: return "1 new message"
        default: return "\(unread) new messages"
        }
    }

    /// How far the visible bottom is above the end of the content, the bottom inset included.
    public static func distance(contentHeight: CGFloat, offsetY: CGFloat, containerHeight: CGFloat, topInset: CGFloat, bottomInset: CGFloat) -> CGFloat {
        // Rows that fit the visible area are always at the bottom, wherever they rest.
        if contentHeight + topInset + bottomInset <= containerHeight { return 0 }
        return contentHeight + bottomInset - containerHeight - offsetY
    }
}

/// The last row each thread showed at its bottom, so a thread opened again marks what came
/// after with a "New" divider (app.md 4.12). Kept for the app's run, per session.
@MainActor
public final class ChatReadMarks {
    public static let shared = ChatReadMarks()
    private var seen: [String: String] = [:]

    public init() {}

    /// The thread showed `rowID` at its bottom.
    public func saw(_ rowID: String?, in key: String) {
        guard let rowID else { return }
        seen[key] = rowID
    }

    public func seen(in key: String) -> String? { seen[key] }

    /// The row the divider goes above: the first row after the one last seen. Nil when nothing
    /// was seen, the seen row is gone (another tail), or nothing came after it.
    public static func divider(rows: [String], seen: String?) -> String? {
        guard let seen, let i = rows.lastIndex(of: seen), i + 1 < rows.count else { return nil }
        return rows[i + 1]
    }
}
