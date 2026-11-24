import Foundation

/// A cron expression in words, for the shapes people write most (app.md 21.2): a time on
/// every day, on weekdays, on some days of the week or on one day of the month, and the
/// presets' steps. Any other shape has no words; the views then show the expression.
public struct CronWords: Equatable, Sendable {
    /// "Daily 07:00", "Mondays 07:00", "Every 5 minutes".
    public let text: String
    /// What the words depend on in the round's zone: nothing (minute steps), the hour's
    /// offset (hourly), or the clock (fixed hours, hour steps).
    let zoneUse: ZoneUse

    enum ZoneUse: Equatable, Sendable {
        case none, minuteOffset, clock
    }

    /// True when the round's zone reads the words differently from the viewer's zone at
    /// `now`, so the words need the zone's name beside them. An unknown zone always does.
    func clock(differsIn roundZone: String, from viewer: TimeZone, at now: Date) -> Bool {
        guard zoneUse != .none else { return false }
        guard let z = TimeZone(identifier: roundZone) else { return true }
        let diff = z.secondsFromGMT(for: now) - viewer.secondsFromGMT(for: now)
        switch zoneUse {
        case .none: return false
        case .minuteOffset: return diff % 3600 != 0
        case .clock: return diff != 0
        }
    }

    private static let days = ["Sundays", "Mondays", "Tuesdays", "Wednesdays", "Thursdays", "Fridays", "Saturdays"]
    private static let shortDays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"]
    private static let dayNames = ["sun", "mon", "tue", "wed", "thu", "fri", "sat"]

    /// The words for a 5-field cron expression, or nil when it is not a common shape.
    public static func words(_ cron: String, locale: Locale = .current) -> CronWords? {
        let f = cron.split(whereSeparator: \.isWhitespace).map { $0.lowercased() }
        guard f.count == 5 else { return nil }
        let (minute, hour, dom, month, dow) = (f[0], f[1], f[2], f[3], f[4])
        guard month == "*" else { return nil }

        // Steps: every N minutes, every hour, every N hours.
        if dom == "*", dow == "*" {
            if hour == "*" {
                if minute == "*" { return CronWords(text: "Every minute", zoneUse: .none) }
                if let n = step(minute, max: 59) {
                    return CronWords(text: localized("Every \(n) minutes"), zoneUse: .none)
                }
                if let m = number(minute, in: 0...59) {
                    return CronWords(text: m == 0 ? "Every hour" : "Every hour at :\(two(m))", zoneUse: .minuteOffset)
                }
                return nil
            }
            if let n = step(hour, max: 23), let m = number(minute, in: 0...59) {
                let text = localized("Every \(n) hours")
                return CronWords(text: m == 0 ? text : "\(text) at :\(two(m))", zoneUse: n == 1 ? .minuteOffset : .clock)
            }
        }

        // A time of day: one minute, one to four hours.
        guard let m = number(minute, in: 0...59), let hours = list(hour, in: 0...23), hours.count <= 4 else { return nil }
        let times = joined(hours.map { clock(hour: $0, minute: m, locale: locale) })

        if dom == "*" {
            guard let set = weekdays(dow) else { return nil }
            let lead: String
            switch set {
            case Set(0...6): lead = "Daily"
            case Set(1...5): lead = "Weekdays"
            case [0, 6]: lead = "Weekends"
            default:
                if set.count == 1, let d = set.first {
                    lead = days[d]
                } else {
                    // Monday first, as a week reads.
                    lead = [1, 2, 3, 4, 5, 6, 0].filter(set.contains).map { shortDays[$0] }.joined(separator: ", ")
                }
            }
            return CronWords(text: "\(lead) \(times)", zoneUse: .clock)
        }
        if dow == "*", let d = number(dom, in: 1...31) {
            return CronWords(text: "Monthly on the \(ordinal(d)) \(times)", zoneUse: .clock)
        }
        return nil
    }

    /// `*/N` with N in 1...max.
    private static func step(_ s: String, max: Int) -> Int? {
        guard s.hasPrefix("*/"), let n = Int(s.dropFirst(2)), (1...max).contains(n) else { return nil }
        return n
    }

    private static func number(_ s: String, in r: ClosedRange<Int>) -> Int? {
        guard let n = Int(s), r.contains(n) else { return nil }
        return n
    }

    /// A list of plain numbers ("9", "9,17"), sorted and without repeats.
    private static func list(_ s: String, in r: ClosedRange<Int>) -> [Int]? {
        var out: [Int] = []
        for part in s.split(separator: ",", omittingEmptySubsequences: false) {
            guard let n = number(String(part), in: r) else { return nil }
            out.append(n)
        }
        return Array(Set(out)).sorted()
    }

    /// The days a day-of-week field names, 0 for Sunday (7 is Sunday too); names in any case;
    /// lists and ranges, no steps.
    static func weekdays(_ s: String) -> Set<Int>? {
        if s == "*" { return Set(0...6) }
        var out = Set<Int>()
        for part in s.split(separator: ",", omittingEmptySubsequences: false) {
            let bounds = part.split(separator: "-", omittingEmptySubsequences: false)
            guard bounds.count <= 2, let a = day(String(bounds[0])) else { return nil }
            if bounds.count == 1 {
                out.insert(a % 7)
            } else {
                guard let b = day(String(bounds[1])), a <= b else { return nil }
                for d in a...b { out.insert(d % 7) }
            }
        }
        return out.isEmpty ? nil : out
    }

    private static func day(_ s: String) -> Int? {
        if let n = Int(s) { return (0...7).contains(n) ? n : nil }
        return dayNames.firstIndex(of: s)
    }

    /// "07:00" in the locale's short time style (a 12-hour locale says "7:00 AM").
    private static func clock(hour: Int, minute: Int, locale: Locale) -> String {
        var cal = Calendar(identifier: .gregorian)
        let utc = TimeZone(identifier: "UTC")!
        cal.timeZone = utc
        let d = cal.date(from: DateComponents(year: 2000, month: 1, day: 1, hour: hour, minute: minute)) ?? Date(timeIntervalSince1970: 0)
        return RoundText.clock(d, timeZone: utc, locale: locale)
    }

    /// "a", "a and b", "a, b and c".
    private static func joined(_ items: [String]) -> String {
        guard items.count > 1 else { return items.first ?? "" }
        return items.dropLast().joined(separator: ", ") + " and " + items.last!
    }

    private static func two(_ n: Int) -> String { n < 10 ? "0\(n)" : "\(n)" }

    private static func ordinal(_ n: Int) -> String {
        let suffix: String
        switch (n % 10, n % 100) {
        case (_, 11...13): suffix = "th"
        case (1, _): suffix = "st"
        case (2, _): suffix = "nd"
        case (3, _): suffix = "rd"
        default: suffix = "th"
        }
        return "\(n)\(suffix)"
    }
}
