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
    /// `clockOffsets` holds, by account key, the hab's clock less this
    /// device's when the reading arrived: its times move by it onto this
    /// device's clock.
    public func update(habID: String, habLabel: String, online: Bool, readings: [UsageAccount], clockOffsets: [String: TimeInterval] = [:]) {
        var changed = false
        if online != onlineHabs.contains(habID) {
            if online { onlineHabs.insert(habID) } else { onlineHabs.remove(habID) }
            changed = true
        }
        let t = now()
        for r in readings {
            let previous = byHab[habID]?[r.key] ?? accounts[r.key]
            let new = Self.reading(r, habID: habID, habLabel: habLabel, receivedAt: t, previous: previous, clockOffset: clockOffsets[r.key])
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
            } else if old.habID != new.habID, Self.emptyDegraded(old), Self.fresh(new, now: now()) {
                // A hab that read nothing has no read time to compare: another
                // hab's fresh numbers replace it, however long ago they were read.
            } else if new.readAt < old.readAt {
                return false
            } else if old.habID != new.habID, Self.emptyDegraded(new), Self.fresh(old, now: now()) {
                // A hab that read nothing never hides another hab's fresh numbers.
                return false
            }
        }
        accounts[new.key] = new
        return true
    }

    /// Degraded with no window: the hab could read nothing for the account.
    private static func emptyDegraded(_ a: UsageAccountReading) -> Bool {
        a.unavailable && a.windows.isEmpty
    }

    /// Numbers worth showing: windows, not degraded, not stale.
    private static func fresh(_ a: UsageAccountReading, now: Date) -> Bool {
        !a.windows.isEmpty && !a.unavailable && !UsageMath.isStale(a, now: now)
    }

    /// The same reading, from any hab: nothing to show, nothing to alert.
    /// Times match within `sameTime`: two connections to one hab measure its
    /// clock offset a little apart.
    private static func sameContent(_ a: UsageAccountReading, _ b: UsageAccountReading) -> Bool {
        a.label == b.label && a.harness == b.harness && a.degraded == b.degraded
            && a.windows.count == b.windows.count && zip(a.windows, b.windows).allSatisfy(sameWindow)
    }

    /// Two dates this close are one time after the clock offset (app.md 14.6).
    static let sameTime: TimeInterval = 2

    static func near(_ a: Date?, _ b: Date?) -> Bool {
        switch (a, b) {
        case (nil, nil): return true
        case let (x?, y?): return abs(x.timeIntervalSince(y)) < sameTime
        default: return false
        }
    }

    private static func sameWindow(_ a: UsageWindowReading, _ b: UsageWindowReading) -> Bool {
        a.name == b.name && a.usedPct == b.usedPct && a.length == b.length
            && near(a.resetsAt, b.resetsAt) && near(a.startsAt, b.startsAt) && near(a.readAt, b.readAt)
    }

    /// An offset under this is measurement noise, not a wrong clock: none is applied.
    static let minOffset: TimeInterval = 1

    /// The shift applied to a hab's times: none for an unknown or tiny offset.
    static func shift(_ clockOffset: TimeInterval?) -> TimeInterval {
        guard let o = clockOffset, abs(o) >= minOffset else { return 0 }
        return o
    }

    /// Converts the hab's account, moving its times onto this device's clock
    /// by `clockOffset`. A window that matches `previous` within `sameTime`
    /// keeps the held window. A window with an unreadable time keeps the
    /// percentage. One with an unreadable read time counts as read now,
    /// unless `previous` held the same window with the same values: then it
    /// keeps that time, so the same reading again is no change.
    static func reading(_ a: UsageAccount, habID: String, habLabel: String, receivedAt: Date, previous: UsageAccountReading? = nil, clockOffset: TimeInterval? = nil) -> UsageAccountReading {
        let shift = Self.shift(clockOffset)
        func local(_ s: String?) -> Date? {
            s.flatMap(Self.parseTime).map { $0.addingTimeInterval(-shift) }
        }
        let windows = a.windows.map { w -> UsageWindowReading in
            let resets = local(w.resetsAt)
            var read = local(w.readAt)
            if read == nil, let old = previous?.windows.first(where: { $0.name == w.name }), old.usedPct == w.usedPct, near(old.resetsAt, resets) {
                read = old.readAt
            }
            let length = w.lengthS.flatMap { $0 > 0 ? TimeInterval($0) : nil }
            let new = UsageWindowReading(name: w.name, usedPct: w.usedPct, resetsAt: resets, readAt: read ?? receivedAt, length: length, startsAt: local(w.startsAt))
            // The same window again, moved a little by a new offset measure, keeps its
            // times: a reconnect is no change to show, alert or write to the widget.
            if let old = previous?.windows.first(where: { $0.name == w.name }), sameWindow(old, new) { return old }
            return new
        }
        var out = UsageAccountReading(key: a.key, label: a.label, harness: a.harness, habID: habID, habLabel: habLabel, windows: windows, receivedAt: receivedAt, degraded: a.degraded)
        // The same reading again from the same hab keeps its arrival time, so
        // an account with no windows ages instead of counting as new each time.
        if let p = previous, p.habID == habID, sameContent(p, out) { out.receivedAt = p.receivedAt }
        return out
    }

    // ISO8601DateFormatter is thread safe; one of each, made once.
    nonisolated(unsafe) private static let fractional: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()
    nonisolated(unsafe) private static let whole: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    nonisolated static func parseTime(_ s: String) -> Date? {
        fractional.date(from: s) ?? whole.date(from: s)
    }

    /// Opens the usage sheet, at an account or at the top.
    public func present(account: String?) {
        presented = Presented(account: account)
    }
}
