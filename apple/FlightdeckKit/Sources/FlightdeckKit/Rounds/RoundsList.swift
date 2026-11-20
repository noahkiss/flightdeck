import Foundation
import HabAPI

/// What a Capcom job may do when its events come in, as its row says it (app.md 21.2,
/// capcom-service.md 9.4, 9.6).
public enum CapcomJobReach: String, Sendable, Hashable {
    /// Its actions run under their own gates: words granted at `allow`, or any job at `all`.
    case actsAlone = "Acts alone"
    /// Each action raises a confirmation first: words granted at `ask`.
    case asksFirst = "Asks first"
    /// It reads and messages only: no words below `all`, or `off`.
    case tellsYou = "Tells you"

    /// The reach from the job's words and the Capcom home's `capcom.event_actions`. An
    /// unknown value (no settings on the home, or a value this app does not know) reads as
    /// `allow`, the hab's built-in default, so the row never claims a check the hab may skip.
    public static func of(words: Set<RoundAction>, eventActions: String?) -> CapcomJobReach {
        switch eventActions {
        case "off": return .tellsYou
        case "all": return .actsAlone
        case "ask": return words.isEmpty ? .tellsYou : .asksFirst
        default: return words.isEmpty ? .tellsYou : .actsAlone
        }
    }
}

/// One row of the Rounds tab (app.md 21.2).
public struct RoundsListRow: Identifiable, Hashable, Sendable {
    public var row: RoundRow
    /// The round's name.
    public var title: String
    /// The first line of the prompt, or the command.
    public var summary: String?
    /// "every 1d · tide-tables · attic"; a paused round leads with "Paused".
    public var meta: String
    /// The right column's first line: the last run's state, or "Next".
    public var status: String?
    public var statusState: RoundRunState?
    /// The right column's second line: when the last run ended, or the next slot.
    public var when: String?
    /// Capcom jobs only.
    public var reach: CapcomJobReach?
    /// The round's hab is online: Unpause needs it.
    public var online = true
    public var id: String { row.id }
    public var paused: Bool { row.mark == .paused }
    public var completed: Bool { row.mark == .completed }
}

/// A section of the Rounds tab.
public struct RoundsListSection: Identifiable, Hashable, Sendable {
    public enum Kind: String, Sendable { case schedule, events, byHand }
    public var kind: Kind
    public var rows: [RoundsListRow]
    public var id: String { kind.rawValue }

    public var title: String {
        switch kind {
        case .schedule: return "On a schedule"
        case .events: return "When something happens"
        case .byHand: return "By hand"
        }
    }

    /// "Capcom handles these" when every row is a Capcom job.
    public var note: String? {
        guard !rows.isEmpty, rows.allSatisfy({ $0.reach != nil }) else { return nil }
        return "Capcom handles these"
    }
}

/// The Rounds tab's list (app.md 21.2): every hab's rounds in one list, split by what starts
/// them. The hab shows in each row, not as a section.
public enum RoundsList {
    /// The section a trigger belongs to.
    public static func kind(_ t: RoundTriggerKind) -> RoundsListSection.Kind {
        switch t {
        case .event, .watch: return .events
        case .manual: return .byHand
        default: return .schedule
        }
    }

    /// The sections with rows, in order: on a schedule, when something happens, by hand.
    /// Within one, `RoundCenter.order`, then the hab's label.
    public static func sections(_ habs: [RoundHab], projectNames: [String: [String: String]] = [:],
                                eventActions: String? = nil, now: Date = Date(),
                                timeZone: TimeZone = .current, locale: Locale = .current) -> [RoundsListSection] {
        var rows: [(RoundHab, RoundRow)] = []
        for h in habs { for r in h.rows { rows.append((h, r)) } }
        rows.sort { a, b in
            if RoundCenter.precedes(a.1.round, b.1.round) { return true }
            if RoundCenter.precedes(b.1.round, a.1.round) { return false }
            return a.0.label < b.0.label
        }
        let kinds: [RoundsListSection.Kind] = [.schedule, .events, .byHand]
        return kinds.compactMap { k in
            let mine = rows.filter { kind($0.1.round.trigger.kind) == k }
            guard !mine.isEmpty else { return nil }
            return RoundsListSection(kind: k, rows: mine.map { h, r in
                row(r, hab: h, project: r.round.project.flatMap { projectNames[h.id]?[$0] ?? $0 },
                    eventActions: eventActions, now: now, timeZone: timeZone, locale: locale)
            })
        }
    }

    /// One row's words.
    public static func row(_ r: RoundRow, hab: RoundHab, project: String?, eventActions: String?, now: Date,
                           timeZone: TimeZone = .current, locale: Locale = .current) -> RoundsListRow {
        let round = r.round
        let job = round.mode == .capcom
        var meta: [String] = []
        if let m = r.mark { meta.append(m.rawValue) }
        if job {
            let outcome = round.lastRun?.outcome?.word
            if let at = round.lastRun.flatMap(ended), outcome != nil {
                meta.append("\(CapcomJobs.outcomeText(outcome)) \(RoundText.time(at, now: now, timeZone: timeZone, locale: locale))")
            } else {
                meta.append(CapcomJobs.outcomeText(outcome))
            }
        }
        meta.append(trigger(round.trigger, now: now, timeZone: timeZone, locale: locale))
        if let project, !project.isEmpty { meta.append(project) }
        meta.append(hab.online ? hab.label : "\(hab.label) (offline)")

        var out = RoundsListRow(row: r, title: round.name, summary: summary(round), meta: meta.joined(separator: " · "))
        out.online = hab.online
        if job { out.reach = CapcomJobReach.of(words: Set(round.actions ?? []), eventActions: eventActions) }
        if r.mark != nil { return out }
        if !job, let last = round.lastRun {
            out.status = RoundText.state(last.state)
            out.statusState = last.state
            if let at = ended(last) ?? last.startedAt.flatMap(RoundText.parse) {
                out.when = relative(at, now: now, locale: locale)
            }
        } else if !job, let n = round.nextSlot {
            out.status = "Next"
            out.when = RoundText.slot(n, roundZone: round.trigger.zone, now: now, timeZone: timeZone, locale: locale)
        }
        return out
    }

    /// The trigger in plain words; an event trigger names its event.
    static func trigger(_ t: RoundTrigger, now: Date, timeZone: TimeZone, locale: Locale) -> String {
        if t.kind == .event, let e = t.event, !e.isEmpty { return "on \(e)" }
        return RoundText.trigger(t, now: now, timeZone: timeZone, locale: locale)
    }

    /// The first line of the prompt, else the command.
    static func summary(_ r: Round) -> String? {
        if let p = RoundText.firstLine(r.prompt) { return p }
        if let c = r.command, !c.isEmpty { return c.joined(separator: " ") }
        return nil
    }

    static func ended(_ run: RoundRun) -> Date? { run.endedAt.flatMap(RoundText.parse) }

    /// "3 days ago", "in 5 minutes"; "just now" under a minute.
    static func relative(_ d: Date, now: Date, locale: Locale) -> String {
        if abs(d.timeIntervalSince(now)) < 60 { return "just now" }
        let f = RelativeDateTimeFormatter()
        f.locale = locale
        f.unitsStyle = .full
        return f.localizedString(for: d, relativeTo: now)
    }
}
