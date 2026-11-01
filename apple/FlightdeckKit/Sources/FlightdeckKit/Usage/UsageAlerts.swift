import FlightdeckShared
import Foundation
import Observation

/// Posts and schedules the app's usage notifications. The app uses
/// `SystemUsageNotifier`; tests record calls.
public protocol UsageNotifier: AnyObject {
    /// Shows a notification now (`at` nil) or at a time. The same id replaces.
    func post(id: String, title: String, body: String, account: String, at: Date?)
    func cancel(id: String)
}

/// Usage warnings and "alert me when free" (app.md 14.3, 14.4), as local
/// notifications. The hab sends no usage push, so these are the only ones.
///
/// - A warning fires once per window and threshold: a window is the account,
///   the window name and its reset time. A reset time that moves by less than
///   ten minutes is the same window (Codex rounds its reset times).
/// - A jump past two thresholds posts the higher one only.
/// - Only a live reading warns: not a stale one, not one whose reset passed.
/// - "Alert me when free" schedules a notification at the reset time. A newer
///   reset time moves it. A fresh reading with room before the reset posts at
///   once and ends the request; a request whose reset time passed ends quietly,
///   since the scheduled notification already fired.
@MainActor
@Observable
public final class UsageAlerts {
    /// Warnings on or off; on by default.
    public var warningsOn: Bool {
        didSet { defaults.set(!warningsOn, forKey: Keys.off) }
    }
    /// Warning thresholds in percent, ascending; default 80 and 95.
    public var thresholds: [Int] {
        didSet {
            let clean = Self.clean(thresholds)
            if clean != thresholds { thresholds = clean; return }
            defaults.set(thresholds, forKey: Keys.thresholds)
        }
    }
    /// Open "alert me when free" requests.
    public private(set) var freeRequests: [FreeRequest]

    public struct FreeRequest: Codable, Hashable, Sendable {
        public var account: String
        public var window: String
        public var resetsAt: Date
    }

    struct Fired: Codable, Hashable {
        var account: String
        var window: String
        var threshold: Int
        var resetsAt: Date?
    }

    enum Keys {
        static let off = "flightdeck.usage.warnings-off"
        static let thresholds = "flightdeck.usage.thresholds"
        static let fired = "flightdeck.usage.fired"
        static let free = "flightdeck.usage.free"
    }

    public static let defaultThresholds = [80, 95]
    /// Reset times closer than this are one window.
    nonisolated static let sameWindow: TimeInterval = 600
    /// A reset time that moved by less than this keeps the scheduled
    /// notification: Codex derives its reset times and they jitter.
    static let rescheduleAfter: TimeInterval = 60

    @ObservationIgnored private var fired: [Fired]
    @ObservationIgnored private let defaults: UserDefaults
    @ObservationIgnored private let notifier: UsageNotifier
    @ObservationIgnored private let now: () -> Date

    public init(notifier: UsageNotifier, defaults: UserDefaults = .standard, now: @escaping () -> Date = Date.init) {
        self.notifier = notifier
        self.defaults = defaults
        self.now = now
        warningsOn = !defaults.bool(forKey: Keys.off)
        let stored = (defaults.array(forKey: Keys.thresholds) as? [Int]).map(Self.clean) ?? []
        thresholds = stored.isEmpty ? Self.defaultThresholds : stored
        fired = Self.load([Fired].self, defaults, Keys.fired) ?? []
        freeRequests = Self.load([FreeRequest].self, defaults, Keys.free) ?? []
    }

    /// Where threshold `i` may move: between its neighbours with 5 points of
    /// room each side, inside 50 to 100, so a stepper never lands one
    /// threshold on another and merges them.
    public static func stepRange(_ i: Int, in t: [Int]) -> ClosedRange<Int> {
        guard t.indices.contains(i) else { return 50...100 }
        var lo = i > 0 ? t[i - 1] + 5 : 50
        var hi = i < t.count - 1 ? t[i + 1] - 5 : 100
        lo = min(max(lo, 1), t[i])
        hi = max(min(hi, 100), t[i])
        return lo...hi
    }

    static func clean(_ t: [Int]) -> [Int] {
        Array(Set(t.filter { (1...100).contains($0) })).sorted()
    }

    // MARK: - Readings

    /// Checks every account after the book changed.
    public func check(_ accounts: [UsageAccountReading]) {
        let t = now()
        if warningsOn {
            // An unavailable account has no number to trust: it warns about nothing.
            for a in accounts where !a.unavailable {
                for w in a.windows { warn(a, w, now: t) }
            }
        }
        // Forget fired marks of windows long gone.
        let before = fired.count
        fired.removeAll { f in f.resetsAt.map { $0 < t.addingTimeInterval(-86400) } ?? false }
        if fired.count != before { save() }
        checkFree(accounts, now: t)
    }

    private func warn(_ a: UsageAccountReading, _ w: UsageWindowReading, now t: Date) {
        if w.resetsAt == nil {
            // A window with no reset time is one window forever: its marks
            // clear when usage falls below them, so the next climb warns again.
            let before = fired.count
            fired.removeAll { $0.account == a.key && $0.window == w.name && $0.resetsAt == nil && w.usedPct < $0.threshold }
            if fired.count != before { save() }
        }
        guard !UsageMath.isStale(w, now: t), !UsageMath.resetPassed(w, now: t) else { return }
        let crossed = thresholds.filter { w.usedPct >= $0 }
        guard let top = crossed.last else { return }
        let new = crossed.filter { th in !fired.contains { $0.matches(a.key, w, th) } }
        guard !new.isEmpty else { return }
        for th in new { fired.append(Fired(account: a.key, window: w.name, threshold: th, resetsAt: w.resetsAt)) }
        save()
        // A threshold below one already warned about posts nothing new.
        guard new.contains(top) else { return }
        let title = "\(a.harnessName) \(w.name) at \(UsageMath.shownPct(w))%"
        var body = top >= 100 || UsageMath.isFull(w) ? "Limit reached." : "\(top)% warning."
        if let r = UsageText.resetsIn(w, now: t) { body += " Resets in \(r)." }
        if a.habLabel.isEmpty == false { body += " On \(a.habLabel)." }
        notifier.post(id: Self.warnID(a.key, w.name), title: title, body: body, account: a.key, at: nil)
    }

    static func warnID(_ account: String, _ window: String) -> String {
        "flightdeck.usage.warn.\(account).\(window)"
    }

    static func freeID(_ account: String, _ window: String) -> String {
        "flightdeck.usage.free.\(account).\(window)"
    }

    // MARK: - Alert me when free

    /// Whether the window offers "Alert me when free": full, with a reset time ahead.
    public func canAlertWhenFree(_ w: UsageWindowReading) -> Bool {
        guard let r = w.resetsAt else { return false }
        return UsageMath.isFull(w) && r > now()
    }

    public func isWaiting(account: String, window: String) -> Bool {
        let t = now()
        return freeRequests.contains { $0.account == account && $0.window == window && $0.resetsAt > t }
    }

    /// Schedules the notification at the window's reset time.
    public func alertWhenFree(_ a: UsageAccountReading, _ w: UsageWindowReading) {
        guard canAlertWhenFree(w), let r = w.resetsAt else { return }
        freeRequests.removeAll { $0.account == a.key && $0.window == w.name }
        freeRequests.append(FreeRequest(account: a.key, window: w.name, resetsAt: r))
        save()
        scheduleFree(a.harnessName, account: a.key, window: w.name, at: r)
    }

    /// Cancels a request.
    public func cancelFree(account: String, window: String) {
        freeRequests.removeAll { $0.account == account && $0.window == window }
        save()
        notifier.cancel(id: Self.freeID(account, window))
    }

    private func scheduleFree(_ harness: String, account: String, window: String, at: Date) {
        notifier.post(id: Self.freeID(account, window), title: "\(harness) \(window) window has reset", body: "Usage has room again.", account: account, at: at)
    }

    private func checkFree(_ accounts: [UsageAccountReading], now t: Date) {
        guard !freeRequests.isEmpty else { return }
        var keep: [FreeRequest] = []
        for var req in freeRequests {
            if req.resetsAt <= t {
                // The scheduled notification has fired.
                continue
            }
            // An unavailable account says nothing about room either.
            guard let a = accounts.first(where: { $0.key == req.account }), !a.unavailable else {
                keep.append(req)
                continue
            }
            // A window missing from the reading says nothing about room: the
            // request stays and ends at its reset time.
            guard let w = a.windows.first(where: { $0.name == req.window }) else {
                keep.append(req)
                continue
            }
            if UsageMath.isFull(w) {
                // Still full. A newer reset time moves the notification; a
                // reading with no reset time leaves it where it is.
                if let r = w.resetsAt, abs(r.timeIntervalSince(req.resetsAt)) >= Self.rescheduleAfter {
                    req.resetsAt = r
                    scheduleFree(a.harnessName, account: a.key, window: req.window, at: r)
                }
                keep.append(req)
                continue
            }
            // A fresh reading with room before the reset time: free now.
            guard !UsageMath.isStale(w, now: t) else {
                keep.append(req)
                continue
            }
            notifier.post(id: Self.freeID(a.key, req.window), title: "\(a.harnessName) \(req.window) has room again", body: "Usage dropped below the limit.", account: a.key, at: nil)
        }
        if keep != freeRequests {
            freeRequests = keep
            save()
        }
    }

    // MARK: - Storage

    private func save() {
        defaults.set(try? JSONEncoder().encode(fired), forKey: Keys.fired)
        defaults.set(try? JSONEncoder().encode(freeRequests), forKey: Keys.free)
    }

    private static func load<T: Decodable>(_ t: T.Type, _ d: UserDefaults, _ key: String) -> T? {
        d.data(forKey: key).flatMap { try? JSONDecoder().decode(T.self, from: $0) }
    }
}

extension UsageAlerts.Fired {
    func matches(_ account: String, _ w: UsageWindowReading, _ threshold: Int) -> Bool {
        guard self.account == account, window == w.name, self.threshold == threshold else { return false }
        switch (resetsAt, w.resetsAt) {
        case (nil, nil): return true
        case let (a?, b?): return abs(a.timeIntervalSince(b)) < UsageAlerts.sameWindow
        default: return false
        }
    }
}
