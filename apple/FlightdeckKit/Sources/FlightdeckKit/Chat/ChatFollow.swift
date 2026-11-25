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

    /// What the scroll geometry reads, rounded to points.
    public struct Sample: Equatable, Sendable {
        /// How far the visible bottom is above the content's end, insets included.
        public var distance: CGFloat
        public var contentHeight: CGFloat
        public var bottomInset: CGFloat
        /// The scroll view's own height. A resize, rotation or Split View change shrinks it and
        /// moves the end away without the owner.
        public var containerHeight: CGFloat

        public init(distance: CGFloat, contentHeight: CGFloat, bottomInset: CGFloat, containerHeight: CGFloat = 0) {
            self.distance = distance
            self.contentHeight = contentHeight
            self.bottomInset = bottomInset
            self.containerHeight = containerHeight
        }
    }

    /// The scroll geometry changed from `old` to `new`.
    ///
    /// The owner's scroll is a finger (motion `.user`), or any move away from the end while the
    /// content and the bars keep their size: a tap on the status bar, Page Up, Home or Cmd-Up,
    /// a VoiceOver scroll, the Mac's scroller or a mouse wheel. Only content, bars or the view
    /// itself changing size move the end without the owner.
    public mutating func scrolled(from old: Sample?, to new: Sample, motion: Motion) -> Action {
        let movedAway = old.map { new.distance > $0.distance + 1 && new.contentHeight == $0.contentHeight && new.bottomInset == $0.bottomInset
            && new.containerHeight == $0.containerHeight } ?? false
        return scrolled(distance: new.distance, motion: movedAway ? .user : motion)
    }

    /// The scroll geometry reads `distance`, with no earlier reading to compare.
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

    /// The bars over the thread changed height (the keyboard, a request card, a taller draft).
    /// They sit in the bottom inset, which the size-change anchor does not see, and the geometry
    /// can read "at the bottom" while the rows sit under the new bar: a following thread scrolls
    /// to its end, unless a finger moves it.
    public func insetChanged(motion: Motion) -> Action {
        following && motion != .user ? .toBottom : .none
    }

    /// How far the rows' end may sit from the bars before a following thread moves.
    public static let bandTolerance: CGFloat = 4

    /// The rows were placed with their end `band` points from the bars (`band(contentHeight:
    /// visible:)`). After a late change of the rows' height the scroll geometry can read "at the
    /// bottom" while the rows rest a whole bottom inset too high (a positive band, an empty gap)
    /// or end a little under the bars (a negative band, within the distance's tolerance). A
    /// following thread scrolls to its end, unless a finger or an animation moves it.
    public func placed(band: CGFloat, motion: Motion) -> Action {
        following && motion == .idle && abs(band) > Self.bandTolerance ? .toBottom : .none
    }

    /// How far the rows' end sits above the bars, in points: positive is an empty gap under the
    /// last row, negative is rows still under the bars. `visible` is the scroll view less its
    /// insets, in the rows' own coordinates (`GeometryProxy.bounds(of: .scrollView)`). Rows
    /// shorter than the view leave no band to close.
    public static func band(contentHeight: CGFloat, visible: CGRect) -> CGFloat {
        guard contentHeight > visible.height else { return 0 }
        return visible.maxY - contentHeight
    }

    /// The rows changed: `old` and `new` are the row ids before and after, in order. Only rows
    /// that `counts` accepts (messages, not tool cards or event lines) add to the unread count.
    public mutating func rowsChanged(from old: [String], to new: [String], counts: (String) -> Bool = { _ in true }) -> Action {
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
            unread += new[(i + 1)...].filter(counts).count
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
    /// `visibleMaxY` is the scroll geometry's visible rectangle's bottom, in content coordinates:
    /// it reaches under the bars, so at the bottom it equals the content's end plus the bottom
    /// inset. Rows that fit the screen rest above that line and read 0.
    public static func distance(contentHeight: CGFloat, visibleMaxY: CGFloat, bottomInset: CGFloat) -> CGFloat {
        max(0, contentHeight + bottomInset - visibleMaxY)
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
