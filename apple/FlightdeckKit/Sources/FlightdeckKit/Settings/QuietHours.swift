import Foundation

/// `capcom.quiet_hours` (capcom-service.md 9.2, 9.9; app.md 20.4): `off`, or a start and an end
/// time of day as "HH:MM-HH:MM" on a 24-hour clock. Quiet hours hold Capcom's pushes, not its
/// work. The value is built from whole hours and minutes, never with a locale's formatter, so a
/// 12-hour locale still writes what the hab takes.
public enum QuietHours: Equatable, Sendable {
    case off
    /// Minutes after midnight, 0 to 1439 each.
    case range(start: Int, end: Int)

    public static let id = "capcom.quiet_hours"
    /// The hab's default, and the range the switch turns on with when none was seen.
    public static let standard = QuietHours.range(start: 22 * 60, end: 7 * 60)

    /// Reads the hab's value; nil for one this app cannot read, which the row never overwrites.
    public static func parse(_ s: String) -> QuietHours? {
        if s == "off" { return .off }
        let parts = s.split(separator: "-", omittingEmptySubsequences: false)
        guard parts.count == 2, let a = clock(parts[0]), let b = clock(parts[1]) else { return nil }
        return .range(start: a, end: b)
    }

    private static func clock(_ p: Substring) -> Int? {
        let c = Array(p)
        guard c.count == 5, c[2] == ":", [0, 1, 3, 4].allSatisfy({ c[$0].isASCII && c[$0].isNumber }),
              let h = Int(String(c[0...1])), let m = Int(String(c[3...4])), h < 24, m < 60 else { return nil }
        return h * 60 + m
    }

    /// The value the hab takes.
    public var value: String {
        switch self {
        case .off: return "off"
        case .range(let a, let b): return Self.hhmm(a) + "-" + Self.hhmm(b)
        }
    }

    static func hhmm(_ minutes: Int) -> String {
        let m = ((minutes % 1440) + 1440) % 1440
        return String(format: "%02d:%02d", m / 60, m % 60)
    }

    /// The same range with a new start or end, in minutes after midnight.
    public func with(start: Int? = nil, end: Int? = nil) -> QuietHours {
        guard case .range(let a, let b) = self else { return self }
        return .range(start: start ?? a, end: end ?? b)
    }

    // MARK: - Time pickers

    /// The calendar the time pickers use: UTC, so no day has a skipped or doubled hour.
    public static let pickerCalendar: Calendar = {
        var c = Calendar(identifier: .gregorian)
        c.timeZone = TimeZone(identifier: "UTC")!
        return c
    }()

    /// A date on a fixed day at a time of day, for a time picker in `pickerCalendar`.
    public static func date(_ minutes: Int) -> Date {
        Date(timeIntervalSinceReferenceDate: TimeInterval(minutes * 60))
    }

    /// The time of day of a picked date, in minutes after midnight.
    public static func minutes(_ d: Date) -> Int {
        let c = pickerCalendar.dateComponents([.hour, .minute], from: d)
        return (c.hour ?? 0) * 60 + (c.minute ?? 0)
    }
}
