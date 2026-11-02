import Foundation
import HabAPI

/// One hab's confirmations (capcom.md 6.1, 6.4), as its host snapshot and the
/// `confirm.requested` and `confirm.closed` events build them.
///
/// - Only a hab whose welcome lists `confirm` fills it; otherwise it stays empty
///   and every event is ignored.
/// - `open` holds what is not closed yet, in arrival order: questions waiting
///   for the owner (`open`), confirmed ones the hab runs (`running`), and
///   records (`record`), which ran at once.
/// - A closed confirmation leaves `open` and goes to the front of `closed`, so a
///   session view can still show what happened. `closed` keeps at most
///   `closedLimit`, newest first, and only within one epoch.
/// - The events carry no revision: they apply in seq order, and the hab sends a
///   confirmation's closed event after its requested one (hab-logic-api.md 3.5).
public struct ConfirmBook: Sendable, Equatable {
    /// The welcome capability that turns confirmations on.
    public static let capability = "confirm"
    /// Closed confirmations kept for the session views.
    public static let closedLimit = 50

    /// Whether the hab on the live (or last) connection serves confirmations.
    public private(set) var enabled = false
    public private(set) var open: [Confirmation] = []
    public private(set) var closed: [Confirmation] = []

    public init() {}

    /// Ids held open before the last reset, within the same epoch: the
    /// `confirm.list` answer after the snapshot says how they ended.
    public private(set) var pending: Set<String> = []

    /// Replaces the open confirmations with a snapshot's. A new epoch, or a hab
    /// without the capability, also drops the closed ones.
    public mutating func reset(_ snapshot: [Confirmation]?, enabled: Bool, newEpoch: Bool) {
        let before = Set(open.map(\.id))
        self.enabled = enabled
        guard enabled else {
            open = []
            closed = []
            pending = []
            return
        }
        if newEpoch { closed = [] }
        pending = newEpoch ? [] : before
        let shut = Set(closed.map(\.id))
        open = (snapshot ?? []).filter { !shut.contains($0.id) }
    }

    /// Merges a `confirm.list {all: true}` answer taken after the snapshot
    /// (the snapshot holds open questions only). Running ones and records join
    /// `open`; a closed one joins `closed` when this book held it open before
    /// the reconnect. `skip` names ids with an event newer than the answer,
    /// which wins. Returns whether anything changed.
    @discardableResult
    public mutating func merge(list: [Confirmation], skip: Set<String>) -> Bool {
        guard enabled else { return false }
        var changed = false
        let known = pending
        pending = []
        for c in list where !skip.contains(c.id) {
            if closed.contains(where: { $0.id == c.id }) { continue }
            if c.isClosed {
                guard known.contains(c.id) || open.contains(where: { $0.id == c.id }) else { continue }
                changed = closed(c) || changed
            } else if !open.contains(where: { $0.id == c.id }) {
                open.append(c)
                changed = true
            }
        }
        if changed {
            closed.sort { Self.time($0.closed) > Self.time($1.closed) }
        }
        return changed
    }

    static func time(_ s: String?) -> Date {
        s.flatMap(UsageBook.parseTime) ?? .distantPast
    }

    /// Applies `confirm.requested` or `confirm.closed`. Returns whether anything
    /// changed; other event types change nothing.
    @discardableResult
    public mutating func apply(_ e: Event) -> Bool {
        guard enabled, e.type == "confirm.requested" || e.type == "confirm.closed" else { return false }
        guard let c = try? e.data.decode(Confirmation.self) else { return false }
        return e.type == "confirm.requested" ? requested(c) : closed(c)
    }

    private mutating func requested(_ c: Confirmation) -> Bool {
        if closed.contains(where: { $0.id == c.id }) { return false }
        if let i = open.firstIndex(where: { $0.id == c.id }) {
            guard open[i] != c else { return false }
            open[i] = c
        } else {
            open.append(c)
        }
        return true
    }

    private mutating func closed(_ c: Confirmation) -> Bool {
        open.removeAll { $0.id == c.id }
        closed.removeAll { $0.id == c.id }
        closed.insert(c, at: 0)
        if closed.count > Self.closedLimit { closed.removeLast(closed.count - Self.closedLimit) }
        return true
    }

    /// The confirmation with this id, open first.
    public func find(_ id: String) -> Confirmation? {
        open.first { $0.id == id } ?? closed.first { $0.id == id }
    }
}

extension Confirmation {
    /// Waits for the owner's answer: open, and not a record.
    public var asks: Bool { state == .open && record != true }

    /// Closed: done, failed, cancelled, expired or unknown.
    public var isClosed: Bool { state != .open && state != .running }

    /// `expires_at` as a date; nil when absent or unreadable.
    public var expiry: Date? { expiresAt.flatMap(UsageBook.parseTime) }
}
