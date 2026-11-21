import Foundation
import HabAPI

/// The usage governor's words (capcom-service.md 10.1), from a catalog's `governor` section.
/// They match gff's "Usage pacing" lines on the status board.
public enum GovernorText {
    /// The pace phrase, or nil when the consumer runs at `ok` on a reading. Capcom's own: "small
    /// model for your messages", "Capcom's own turns wait until 15:04", or both joined by "; ".
    /// The owner's messages never wait on a current hab, so no Capcom phrase says they are
    /// paused. Another consumer's: "small model", "event turns wait until 15:04". A wait of
    /// user turns (a group's, or an older hab's Capcom): "Pacing usage until 15:04". With no
    /// reading: "unpaced".
    public static func pace(_ k: GovernorConsumer, now: Date, calendar: Calendar = .current, locale: Locale = .current) -> String? {
        if k.basis == "none" { return "unpaced" }
        if k.pace == "wait" { return "Pacing usage" + until(k.until, now: now, calendar: calendar, locale: locale) }
        let capcom = k.id == "capcom"
        var parts: [String] = []
        if k.pace == "small" { parts.append(capcom ? "small model for your messages" : "small model") }
        if k.eventPace == "wait" {
            parts.append((capcom ? "Capcom's own turns wait" : "event turns wait") + until(k.eventUntil, now: now, calendar: calendar, locale: locale))
        }
        return parts.isEmpty ? nil : parts.joined(separator: "; ")
    }

    /// What the pace reads: "40% of the week used, share 30%", "$1.20 of $5.00 today", or
    /// "12.3k tokens today" when unpaced. Nil for a basis the app does not know.
    public static func use(_ k: GovernorConsumer) -> String? {
        switch k.basis {
        case "plan":
            return "\(k.usedPct ?? 0)% of the week used, share \(k.sharePct ?? 0)%"
        case "usd":
            return "\(dollars(k.spentCents ?? 0)) of \(dollars(k.capCents ?? 0)) today"
        case "none":
            return "\(tokens((k.inputTokens ?? 0) + (k.outputTokens ?? 0))) tokens today"
        default:
            return nil
        }
    }

    /// One consumer's line, or nil when it runs at `ok` on a reading: the pace, then the use.
    /// "Small model for your messages · 40% of the week used, share 30%", "Capcom's own turns
    /// wait until 15:04 · $5.10 of $5.00 today", "Unpaced: 12.3k tokens today".
    public static func line(_ k: GovernorConsumer, now: Date, calendar: Calendar = .current, locale: Locale = .current) -> String? {
        guard let p = pace(k, now: now, calendar: calendar, locale: locale) else { return nil }
        let head = p.prefix(1).uppercased() + p.dropFirst()
        guard let u = use(k) else { return head }
        return k.basis == "none" ? "\(head): \(u)" : "\(head) · \(u)"
    }

    /// " until 15:04" today, " until Mon 15:04" on another day, in local time; "" when the
    /// time does not parse.
    static func until(_ at: String?, now: Date, calendar: Calendar, locale: Locale) -> String {
        guard let at, let t = BoardRows.parseStamp(at) else { return "" }
        let tz = calendar.timeZone
        if calendar.isDate(t, inSameDayAs: now) {
            return " until " + t.formatted(Date.FormatStyle(date: .omitted, time: .shortened, locale: locale, calendar: calendar, timeZone: tz))
        }
        let style = Date.FormatStyle(locale: locale, calendar: calendar, timeZone: tz).weekday(.abbreviated).hour().minute()
        return " until " + t.formatted(style)
    }

    static func dollars(_ cents: Int) -> String {
        String(format: "$%.2f", Double(cents) / 100)
    }

    static func tokens(_ n: Int) -> String {
        if n >= 1_000_000 { return String(format: "%.1fM", Double(n) / 1e6) }
        if n >= 1_000 { return String(format: "%.1fk", Double(n) / 1e3) }
        return String(n)
    }
}

/// One row of "Usage pacing" on the usage sheet: a consumer on a hab that is not at `ok`.
public struct GovernorRow: Identifiable, Equatable, Sendable {
    public var habID: String
    public var habLabel: String
    /// `capcom`, or `group:<id>`.
    public var consumer: String
    public var text: String
    public var id: String { habID + "/" + consumer }

    /// "Capcom", or the group's id.
    public var title: String {
        consumer == "capcom" ? "Capcom" : consumer
    }
}
