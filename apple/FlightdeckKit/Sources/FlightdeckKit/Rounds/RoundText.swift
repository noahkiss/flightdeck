import Foundation
import HabAPI

/// The words the round views show (rounds.md 4.1, 10.3).
public enum RoundText {
    /// The presets a cron expression may come from, as the hab names them (rounds.md 4.1).
    public static let presets: [(name: String, cron: String)] = [
        ("1m", "* * * * *"), ("2m", "*/2 * * * *"), ("5m", "*/5 * * * *"), ("10m", "*/10 * * * *"),
        ("15m", "*/15 * * * *"), ("20m", "*/20 * * * *"), ("30m", "*/30 * * * *"), ("1h", "0 * * * *"),
        ("2h", "0 */2 * * *"), ("3h", "0 */3 * * *"), ("4h", "0 */4 * * *"), ("6h", "0 */6 * * *"),
        ("8h", "0 */8 * * *"), ("12h", "0 */12 * * *"), ("1d", "0 0 * * *"),
    ]

    /// The preset a cron expression compiles from, or nil.
    public static func preset(cron: String) -> String? {
        let c = cron.split(whereSeparator: \.isWhitespace).joined(separator: " ")
        return presets.first { $0.cron == c }?.name
    }

    /// The trigger in plain words, as the hab's catalog writes it: "every 1h", "every 1d in
    /// Europe/Berlin", "cron 0 9 * * 1-5 in Europe/Berlin", "once at <time>", "by hand".
    public static func trigger(_ t: RoundTrigger, timeZone: TimeZone = .current, locale: Locale = .current) -> String {
        switch t.kind {
        case .cron:
            let cron = t.cron ?? ""
            let zone = t.zone.flatMap { $0.isEmpty ? nil : $0 }
            if let p = preset(cron: cron) {
                if p.hasSuffix("m") || zone == nil { return "every \(p)" }
                return "every \(p) in \(zone!)"
            }
            return zone.map { "cron \(cron) in \($0)" } ?? "cron \(cron)"
        case .once:
            guard let at = t.at, let d = parse(at) else { return "once at \(t.at ?? "?")" }
            return "once at \(time(d, now: d, timeZone: timeZone, locale: locale))"
        case .manual:
            return "by hand"
        default:
            return t.kind.rawValue
        }
    }

    /// A slot in the viewer's zone, with the round's zone beside it when its clock differs
    /// there: "today 09:00", "tomorrow 15:00 (09:00 America/Chicago)" (rounds.md 4.1).
    public static func slot(_ stamp: String, roundZone: String?, now: Date = Date(),
                            timeZone: TimeZone = .current, locale: Locale = .current) -> String {
        guard let d = parse(stamp) else { return stamp }
        let mine = time(d, now: now, timeZone: timeZone, locale: locale)
        guard let name = roundZone, !name.isEmpty, let z = TimeZone(identifier: name),
              z.secondsFromGMT(for: d) != timeZone.secondsFromGMT(for: d) else { return mine }
        return "\(mine) (\(clock(d, timeZone: z, locale: locale)) \(name))"
    }

    /// "today 09:00", "tomorrow 09:00", "yesterday 09:00", else the short date and time.
    static func time(_ d: Date, now: Date, timeZone: TimeZone, locale: Locale) -> String {
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = timeZone
        let days = cal.dateComponents([.day], from: cal.startOfDay(for: now), to: cal.startOfDay(for: d)).day ?? 0
        let c = clock(d, timeZone: timeZone, locale: locale)
        switch days {
        case 0: return "today \(c)"
        case 1: return "tomorrow \(c)"
        case -1: return "yesterday \(c)"
        default:
            let f = DateFormatter()
            f.locale = locale
            f.timeZone = timeZone
            f.setLocalizedDateFormatFromTemplate("EEE d MMM")
            return "\(f.string(from: d)) \(c)"
        }
    }

    static func clock(_ d: Date, timeZone: TimeZone, locale: Locale) -> String {
        let f = DateFormatter()
        f.locale = locale
        f.timeZone = timeZone
        f.timeStyle = .short
        f.dateStyle = .none
        return f.string(from: d)
    }

    /// A run state as the views say it.
    public static func state(_ s: RoundRunState) -> String {
        switch s {
        case .queued: return "Queued"
        case .waiting: return "Waiting"
        case .running: return "Running"
        case .blocked: return "Blocked"
        case .succeeded: return "Succeeded"
        case .failed: return "Failed"
        case .timedOut: return "Timed out"
        case .skipped: return "Skipped"
        case .takenOver: return "Taken over"
        default: return s.rawValue
        }
    }

    /// A run's length, "4 s", "3 min", "1 h 5 min"; nil before it started or while it runs.
    public static func duration(_ run: RoundRun) -> String? {
        guard let s = run.startedAt.flatMap(parse), let e = run.endedAt.flatMap(parse) else { return nil }
        let secs = max(0, Int(e.timeIntervalSince(s).rounded()))
        if secs < 60 { return "\(secs) s" }
        let mins = secs / 60
        if mins < 60 { return "\(mins) min" }
        return mins % 60 == 0 ? "\(mins / 60) h" : "\(mins / 60) h \(mins % 60) min"
    }

    /// The first line of a run's result with text on it.
    public static func firstLine(_ s: String?) -> String? {
        s?.split(whereSeparator: \.isNewline).lazy.map { $0.trimmingCharacters(in: .whitespaces) }.first { !$0.isEmpty }
    }

    static func parse(_ s: String) -> Date? { BoardRows.parseStamp(s) }
}
