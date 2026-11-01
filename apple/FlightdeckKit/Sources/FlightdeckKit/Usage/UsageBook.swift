import FlightdeckShared
import Foundation
import HabAPI
import Observation
import os

/// Every harness account's newest usage reading, merged over all habs by
/// account key (app.md 14.1). The newest reading wins, by `read_at`, from
/// any hab, the same hab included. The same reading from two habs (one hab
/// reached two ways) stays with the hab that has it, unless that hab is
/// offline and the other online. Readings outlive the connection:
/// an offline hab's accounts stay with their age, and the book starts from
/// the last widget snapshot.
@MainActor
@Observable
public final class UsageBook {
    public private(set) var accounts: [String: UsageAccountReading] = [:]
    /// Habs online now; an account from any other hab shows "hab offline".
    public private(set) var onlineHabs: Set<String> = []
    /// Each hab's latest reading per account, so a tie can move to another
    /// hab at once when the hab holding it goes offline.
    @ObservationIgnored private var byHab: [String: [String: UsageAccountReading]] = [:]
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
            let previous = byHab[habID]?[r.key] ?? accounts[r.key]
            let new = Self.reading(r, habID: habID, habLabel: habLabel, receivedAt: t, previous: previous)
            byHab[habID, default: [:]][r.key] = new
            if merge(new) { changed = true }
        }
        if !online && rehome(from: habID) { changed = true }
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
        byHab[habID] = nil
        if accounts.count != before { onChange?(Array(accounts.values)) }
    }

    /// Keeps only the accounts of these habs: at launch, seeded accounts of a
    /// hab no longer configured go.
    public func retain(habIDs: Set<String>) {
        let before = accounts.count
        accounts = accounts.filter { habIDs.contains($0.value.habID) }
        byHab = byHab.filter { habIDs.contains($0.key) }
        onlineHabs.formIntersection(habIDs)
        if accounts.count != before { onChange?(Array(accounts.values)) }
    }

    /// The hab went offline: each account it holds moves to an online hab
    /// that holds the same reading. Returns whether any moved.
    private func rehome(from habID: String) -> Bool {
        var moved = false
        for (k, a) in accounts where a.habID == habID {
            let other = byHab.first { h, readings in
                h != habID && onlineHabs.contains(h) && readings[k].map { Self.sameContent($0, a) } == true
            }
            if let r = other?.value[k] {
                accounts[k] = r
                moved = true
            }
        }
        return moved
    }

    /// Merges one reading. Returns whether the book changed.
    @discardableResult
    func merge(_ new: UsageAccountReading) -> Bool {
        if let old = accounts[new.key] {
            if Self.sameContent(old, new) {
                // A tie moves only from an offline hab to an online one.
                guard old.habID != new.habID, !onlineHabs.contains(old.habID), onlineHabs.contains(new.habID) else { return false }
            } else if new.readAt < old.readAt {
                return false
            }
        }
        accounts[new.key] = new
        return true
    }

    /// The same reading, from any hab: nothing to show, nothing to alert.
    private static func sameContent(_ a: UsageAccountReading, _ b: UsageAccountReading) -> Bool {
        a.label == b.label && a.harness == b.harness && a.windows == b.windows
    }

    /// Converts the hab's account. A window with an unreadable time keeps the
    /// percentage. One with an unreadable read time counts as read now,
    /// unless `previous` held the same window with the same values: then it
    /// keeps that time, so the same reading again is no change.
    static func reading(_ a: UsageAccount, habID: String, habLabel: String, receivedAt: Date, previous: UsageAccountReading? = nil) -> UsageAccountReading {
        let windows = a.windows.map { w -> UsageWindowReading in
            let resets = w.resetsAt.flatMap(Self.parseTime)
            var read = Self.parseTime(w.readAt)
            if read == nil, let old = previous?.windows.first(where: { $0.name == w.name }), old.usedPct == w.usedPct, old.resetsAt == resets {
                read = old.readAt
            }
            return UsageWindowReading(name: w.name, usedPct: w.usedPct, resetsAt: resets, readAt: read ?? receivedAt)
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
