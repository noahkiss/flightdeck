import Foundation

// Usage readings as the app keeps them and the widget reads them (app.md 14,
// 15). Foundation only, so the widget extension stays light.

/// One limit window of an account, as last read.
public struct UsageWindowReading: Codable, Hashable, Sendable {
    /// "5h", "7d", "7d opus", "spend monthly", or a model name.
    public var name: String
    /// Used, in whole percent. It may pass 100; the app caps what it shows.
    public var usedPct: Int
    public var resetsAt: Date?
    public var readAt: Date

    public init(name: String, usedPct: Int, resetsAt: Date?, readAt: Date) {
        self.name = name
        self.usedPct = usedPct
        self.resetsAt = resetsAt
        self.readAt = readAt
    }
}

/// The newest reading of one harness account, merged over every hab.
public struct UsageAccountReading: Codable, Hashable, Sendable, Identifiable {
    public var key: String
    /// The plan name, such as "Max 5x".
    public var label: String?
    public var harness: String
    /// The hab connection the reading came from, and that hab's label.
    public var habID: String
    public var habLabel: String
    public var windows: [UsageWindowReading]
    /// When the app received the reading; the reading time of an account with no windows.
    public var receivedAt: Date

    public var id: String { key }

    public init(key: String, label: String?, harness: String, habID: String, habLabel: String, windows: [UsageWindowReading], receivedAt: Date) {
        self.key = key
        self.label = label
        self.harness = harness
        self.habID = habID
        self.habLabel = habLabel
        self.windows = windows
        self.receivedAt = receivedAt
    }

    /// When the hab read the account: its newest window, else when the app got it.
    public var readAt: Date {
        windows.map(\.readAt).max() ?? receivedAt
    }

    /// The harness's display name.
    public var harnessName: String { UsageText.harnessName(harness) }
}

/// Window math: lengths, pace, reset state and staleness (app.md 14.2, 14.5).
public enum UsageMath {
    /// A reading older than this is stale, or older than its window when that is shorter.
    public static let staleAfter: TimeInterval = 3600

    /// The window's length, read from its name: the first word like `5h`,
    /// `30m`, `7d` or `2w`, or a period word (`daily`, `weekly`, `monthly`).
    /// The hab sends no length (task-bw97v7dk), so a model name alone has none.
    public static func length(of name: String) -> TimeInterval? {
        for word in name.lowercased().split(whereSeparator: { $0 == " " || $0 == "_" || $0 == "-" }) {
            switch word {
            case "hourly": return 3600
            case "daily": return 86400
            case "weekly": return 7 * 86400
            case "monthly": return 30 * 86400
            default: break
            }
            guard let unit = word.last, let n = Double(word.dropLast()), n > 0 else { continue }
            switch unit {
            case "m": return n * 60
            case "h": return n * 3600
            case "d": return n * 86400
            case "w": return n * 7 * 86400
            default: continue
            }
        }
        return nil
    }

    /// The used percentage as shown: 0 to 100.
    public static func shownPct(_ w: UsageWindowReading) -> Int {
        min(100, max(0, w.usedPct))
    }

    public static func isFull(_ w: UsageWindowReading) -> Bool {
        w.usedPct >= 100
    }

    /// The reset time passed with no reading since.
    public static func resetPassed(_ w: UsageWindowReading, now: Date) -> Bool {
        guard let r = w.resetsAt else { return false }
        return r <= now && w.readAt < r
    }

    /// The percentage that counts now: zero once the window reset with no new reading.
    public static func effectivePct(_ w: UsageWindowReading, now: Date) -> Int {
        resetPassed(w, now: now) ? 0 : shownPct(w)
    }

    /// The share of the window already gone, 0 to 1: where even spending
    /// would put usage. Nil without a length or a reset time.
    public static func pace(_ w: UsageWindowReading, now: Date) -> Double? {
        guard let len = length(of: w.name), let r = w.resetsAt, !resetPassed(w, now: now) else { return nil }
        let left = r.timeIntervalSince(now)
        return min(1, max(0, 1 - left / len))
    }

    /// Used ahead of the even pace.
    public static func overPace(_ w: UsageWindowReading, now: Date) -> Bool {
        guard let p = pace(w, now: now), !isFull(w) else { return false }
        return Double(shownPct(w)) > p * 100 + 0.5
    }

    /// Older than an hour, or than the window when that is shorter.
    public static func isStale(_ w: UsageWindowReading, now: Date) -> Bool {
        let limit = min(staleAfter, length(of: w.name) ?? staleAfter)
        return now.timeIntervalSince(w.readAt) > limit
    }

    public static func isStale(_ a: UsageAccountReading, now: Date) -> Bool {
        if a.windows.isEmpty { return now.timeIntervalSince(a.receivedAt) > staleAfter }
        return a.windows.contains { isStale($0, now: now) }
    }

    /// The window nearest its limit, for the footer bar and the widget.
    public static func tightest(_ a: UsageAccountReading, now: Date) -> UsageWindowReading? {
        a.windows.max { x, y in
            let ex = effectivePct(x, now: now), ey = effectivePct(y, now: now)
            if ex != ey { return ex < ey }
            // Equal: the one that resets later binds longer.
            return (x.resetsAt ?? .distantPast) < (y.resetsAt ?? .distantPast)
        }
    }

    /// Accounts ordered tightest first, then by key.
    public static func byTightest(_ accounts: [UsageAccountReading], now: Date) -> [UsageAccountReading] {
        accounts.sorted { a, b in
            let pa = tightest(a, now: now).map { effectivePct($0, now: now) } ?? -1
            let pb = tightest(b, now: now).map { effectivePct($0, now: now) } ?? -1
            if pa != pb { return pa > pb }
            return a.key < b.key
        }
    }
}

/// Words for usage (app.md 14.2).
public enum UsageText {
    public static func harnessName(_ h: String) -> String {
        switch h {
        case "claude": return "Claude"
        case "codex": return "Codex"
        case "opencode": return "opencode"
        default: return h
        }
    }

    /// "1h 58m", "4d 23h", "12m", "now".
    public static func duration(_ s: TimeInterval) -> String {
        let m = Int(max(0, s) / 60)
        if m < 1 { return "now" }
        if m < 60 { return "\(m)m" }
        let h = m / 60
        if h < 24 { return "\(h)h \(m % 60)m" }
        return "\(h / 24)d \(h % 24)h"
    }

    /// The time to reset, or nil without a reset time.
    public static func resetsIn(_ w: UsageWindowReading, now: Date) -> String? {
        guard let r = w.resetsAt else { return nil }
        return duration(r.timeIntervalSince(now))
    }

    /// The line under an account's rows: a reached limit first, then over pace, then on pace.
    public static func status(_ a: UsageAccountReading, now: Date) -> String? {
        let live = a.windows.filter { !UsageMath.resetPassed($0, now: now) }
        if let full = live.first(where: UsageMath.isFull) {
            if let r = resetsIn(full, now: now) { return "\(full.name) limit reached · resets in \(r)" }
            return "\(full.name) limit reached"
        }
        if let over = live.first(where: { UsageMath.overPace($0, now: now) }) {
            return "Over pace on \(over.name)"
        }
        if live.contains(where: { UsageMath.pace($0, now: now) != nil }) { return "On pace" }
        return nil
    }

    /// The short state a widget shows for one window.
    public static func short(_ w: UsageWindowReading, now: Date) -> String {
        if UsageMath.resetPassed(w, now: now) { return "Reset" }
        if UsageMath.isFull(w) { return "Limit reached" }
        if UsageMath.overPace(w, now: now) { return "Over pace" }
        if UsageMath.pace(w, now: now) != nil { return "On pace" }
        return "\(UsageMath.shownPct(w))% used"
    }
}

/// What the app hands the widgets through the App Group (app.md 15.1): the
/// last known readings and when they were written.
public struct UsageSnapshot: Codable, Equatable, Sendable {
    public static let version = 1
    public var version: Int
    public var writtenAt: Date
    public var accounts: [UsageAccountReading]

    public init(writtenAt: Date, accounts: [UsageAccountReading]) {
        self.version = Self.version
        self.writtenAt = writtenAt
        self.accounts = accounts
    }

    /// The App Group defaults key.
    public static let defaultsKey = "flightdeck.usage.snapshot"
    /// The widget kind, for reloads.
    public static let widgetKind = "flightdeck.usage"

    public func encoded() throws -> Data {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .iso8601
        e.outputFormatting = [.sortedKeys]
        return try e.encode(self)
    }

    /// Reads a snapshot; nil for missing data, bad data or another version.
    public static func decode(_ data: Data?) -> UsageSnapshot? {
        guard let data else { return nil }
        let d = JSONDecoder()
        d.dateDecodingStrategy = .iso8601
        guard let s = try? d.decode(UsageSnapshot.self, from: data), s.version == version else { return nil }
        return s
    }

    public static func load(_ defaults: UserDefaults = SharedSettings.defaults) -> UsageSnapshot? {
        decode(defaults.data(forKey: defaultsKey))
    }

    /// Writes the snapshot; returns false when it equals what is stored.
    @discardableResult
    public func save(_ defaults: UserDefaults = SharedSettings.defaults) -> Bool {
        guard let data = try? encoded() else { return false }
        if let old = Self.load(defaults), old.accounts == accounts { return false }
        defaults.set(data, forKey: Self.defaultsKey)
        return true
    }
}

/// The link that opens the usage sheet: `flightdeck://usage[?account=<key>]`.
public enum UsageLink {
    public static func url(account: String? = nil) -> URL {
        var c = URLComponents()
        c.scheme = "flightdeck"
        c.host = "usage"
        if let account { c.queryItems = [URLQueryItem(name: "account", value: account)] }
        return c.url!
    }

    /// The account a usage link names: `.some(nil)` for the sheet with no account.
    public static func account(_ url: URL) -> String?? {
        guard url.scheme == "flightdeck", url.host == "usage" else { return nil }
        let key = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems?.first { $0.name == "account" }?.value
        return .some(key)
    }
}
