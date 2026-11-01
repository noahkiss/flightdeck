import FlightdeckShared
import Foundation
import HabAPI
import Observation
import os

/// Every harness account's newest usage reading, merged over all habs by
/// account key (app.md 14.1). The newest reading wins; a hab's own later
/// report always replaces its earlier one. Readings outlive the connection:
/// an offline hab's accounts stay with their age, and the book starts from
/// the last widget snapshot.
@MainActor
@Observable
public final class UsageBook {
    public private(set) var accounts: [String: UsageAccountReading] = [:]
    /// Habs online now; an account from any other hab shows "hab offline".
    public private(set) var onlineHabs: Set<String> = []
    /// The account the usage sheet opens at; the sheet shows while set.
    public var presented: Presented?

    public struct Presented: Identifiable, Equatable {
        public var account: String?
        public var id: String { account ?? "" }
    }

    /// Called after every change, with the accounts.
    @ObservationIgnored public var onChange: (([UsageAccountReading]) -> Void)?
    @ObservationIgnored private let now: () -> Date
    @ObservationIgnored private let log = Logger(subsystem: "com.flightdeck.app", category: "usage")

    public init(seed: [UsageAccountReading] = [], now: @escaping () -> Date = Date.init) {
        self.now = now
        for a in seed { accounts[a.key] = a }
    }

    /// Accounts, tightest first.
    public var ordered: [UsageAccountReading] {
        UsageMath.byTightest(Array(accounts.values), now: now())
    }

    public func isOnline(_ a: UsageAccountReading) -> Bool {
        onlineHabs.contains(a.habID)
    }

    /// A hab's readings: the whole `usage.list` answer, or the readings it
    /// holds after events. Accounts it no longer reports keep their last reading.
    public func update(habID: String, habLabel: String, online: Bool, readings: [UsageAccount]) {
        var changed = false
        if online != onlineHabs.contains(habID) {
            if online { onlineHabs.insert(habID) } else { onlineHabs.remove(habID) }
            changed = true
        }
        let t = now()
        for r in readings {
            let new = Self.reading(r, habID: habID, habLabel: habLabel, receivedAt: t)
            if merge(new) { changed = true }
        }
        // A label change (config or welcome) renames the hab on its cards.
        for (k, a) in accounts where a.habID == habID && a.habLabel != habLabel {
            accounts[k]?.habLabel = habLabel
            changed = true
        }
        if changed { onChange?(Array(accounts.values)) }
    }

    /// A hab was removed from the app: its accounts go too.
    public func forget(habID: String) {
        let before = accounts.count
        accounts = accounts.filter { $0.value.habID != habID }
        onlineHabs.remove(habID)
        if accounts.count != before { onChange?(Array(accounts.values)) }
    }

    /// Drops the accounts of every hab whose id starts with `prefix`.
    public func forget(habIDPrefix prefix: String) {
        let before = accounts.count
        accounts = accounts.filter { !$0.value.habID.hasPrefix(prefix) }
        if accounts.count != before { onChange?(Array(accounts.values)) }
    }

    /// Merges one reading. Returns whether the book changed.
    @discardableResult
    func merge(_ new: UsageAccountReading) -> Bool {
        if let old = accounts[new.key] {
            if Self.sameContent(old, new) { return false }
            // The newest reading wins, whichever hab sent it.
            if new.readAt < old.readAt { return false }
        }
        accounts[new.key] = new
        return true
    }

    /// The same reading again: nothing to show, nothing to alert.
    private static func sameContent(_ a: UsageAccountReading, _ b: UsageAccountReading) -> Bool {
        a.habID == b.habID && a.label == b.label && a.harness == b.harness && a.windows == b.windows
    }

    /// Converts the hab's account. A window with an unreadable time keeps the
    /// percentage; one with an unreadable read time counts as read now.
    static func reading(_ a: UsageAccount, habID: String, habLabel: String, receivedAt: Date) -> UsageAccountReading {
        let windows = a.windows.map { w in
            UsageWindowReading(
                name: w.name,
                usedPct: w.usedPct,
                resetsAt: w.resetsAt.flatMap(Self.parseTime),
                readAt: Self.parseTime(w.readAt) ?? receivedAt)
        }
        return UsageAccountReading(key: a.key, label: a.label, harness: a.harness, habID: habID, habLabel: habLabel, windows: windows, receivedAt: receivedAt)
    }

    static func parseTime(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }

    /// Opens the usage sheet, at an account or at the top.
    public func present(account: String?) {
        presented = Presented(account: account)
    }
}
