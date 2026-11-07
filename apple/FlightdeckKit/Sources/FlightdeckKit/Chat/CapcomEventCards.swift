import FlightdeckShared
import Foundation
import HabAPI
import Observation
import os

// Capcom's calendar and reminder cards (capcom-service.md 8.5): one Add tap, never silent.
// Add claims the card first with `capcom.card.answer add`; only when the hab takes it does the
// device write through EventKit. A `conflict` (another device answered) writes nothing.

/// A calendar event or a reminder.
public enum CapcomEventKind: String, Sendable {
    case calendar
    case reminder
}

/// What a card would add, with its times read into dates.
public struct CapcomEventDraft: Equatable, Sendable {
    public var kind: CapcomEventKind
    public var title: String
    public var start: Date?
    public var end: Date?
    public var allDay: Bool
    /// A reminder's due time; with `dueIsDate` a day with no time.
    public var due: Date?
    public var dueIsDate: Bool
    public var location: String?
    public var notes: String?
    public var url: URL?
    public var timeZone: TimeZone

    public init(kind: CapcomEventKind, title: String, start: Date? = nil, end: Date? = nil, allDay: Bool = false, due: Date? = nil,
                dueIsDate: Bool = false, location: String? = nil, notes: String? = nil, url: URL? = nil, timeZone: TimeZone = .current) {
        self.kind = kind
        self.title = title
        self.start = start
        self.end = end
        self.allDay = allDay
        self.due = due
        self.dueIsDate = dueIsDate
        self.location = location
        self.notes = notes
        self.url = url
        self.timeZone = timeZone
    }

    /// The draft of a card's event, or nil when the item is no calendar or reminder card.
    public static func of(_ item: CapcomItem) -> CapcomEventDraft? {
        guard let card = item.card, let e = card.event, let kind = CapcomEventKind(rawValue: card.type) else { return nil }
        return of(e, kind: kind)
    }

    public static func of(_ e: CapcomCardEvent, kind: CapcomEventKind) -> CapcomEventDraft {
        let tz = e.tz.flatMap(TimeZone.init(identifier:)) ?? .current
        var d = CapcomEventDraft(kind: kind, title: e.title, allDay: e.allDay == true, location: nonEmpty(e.location),
                                 notes: nonEmpty(e.notes), url: nonEmpty(e.url).flatMap(URL.init(string:)), timeZone: tz)
        if let s = e.start, let t = CapcomTime.parse(s, tz) {
            d.start = t.date
            if t.isDate { d.allDay = true }
        }
        if let s = e.end, let t = CapcomTime.parse(s, tz) {
            d.end = t.date
        }
        if let s = e.due, let t = CapcomTime.parse(s, tz) {
            d.due = t.date
            d.dueIsDate = t.isDate
        }
        return d
    }

    /// When the event ends: an all-day event's last day runs to its midnight. A reminder never
    /// ends.
    public var endsAt: Date? {
        guard kind == .calendar else { return nil }
        guard let last = end ?? start else { return nil }
        if allDay {
            var cal = Calendar(identifier: .gregorian)
            cal.timeZone = timeZone
            return cal.date(byAdding: .day, value: 1, to: cal.startOfDay(for: last))
        }
        return last
    }

    /// The event has ended: the card shows "Past" and no Add.
    public func isPast(_ now: Date) -> Bool {
        guard let e = endsAt else { return false }
        return e <= now
    }

    /// The "when" line in this device's time zone.
    public func whenText(locale: Locale = .current, timeZone here: TimeZone = .current) -> String? {
        let f = DateFormatter()
        f.locale = locale
        f.timeZone = allDay || (kind == .reminder && dueIsDate) ? timeZone : here
        switch kind {
        case .calendar:
            guard let s = start else { return nil }
            if allDay {
                f.dateStyle = .medium
                f.timeStyle = .none
                let a = f.string(from: s)
                guard let e = end, !Calendar.current.isDate(e, inSameDayAs: s) else { return "\(a), all day" }
                return "\(a) to \(f.string(from: e)), all day"
            }
            f.dateStyle = .medium
            f.timeStyle = .short
            let a = f.string(from: s)
            guard let e = end else { return a }
            var cal = Calendar(identifier: .gregorian)
            cal.timeZone = here
            if cal.isDate(e, inSameDayAs: s) {
                f.dateStyle = .none
            }
            return "\(a) to \(f.string(from: e))"
        case .reminder:
            guard let d = due else { return nil }
            f.dateStyle = .medium
            f.timeStyle = dueIsDate ? .none : .short
            return "Due \(f.string(from: d))"
        }
    }

    static func nonEmpty(_ s: String?) -> String? {
        guard let s, !s.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { return nil }
        return s
    }
}

/// The card's times: RFC 3339, or a date with no time.
enum CapcomTime {
    static func parse(_ s: String, _ tz: TimeZone) -> (date: Date, isDate: Bool)? {
        if s.count == 10, s.wholeMatch(of: #/\d{4}-\d{2}-\d{2}/#) != nil {
            let f = DateFormatter()
            f.locale = Locale(identifier: "en_US_POSIX")
            f.calendar = Calendar(identifier: .gregorian)
            f.timeZone = tz
            f.dateFormat = "yyyy-MM-dd"
            return f.date(from: s).map { ($0, true) }
        }
        return PushMeta.parseDate(s).map { ($0, false) }
    }
}

/// Writes a card's event on this device: EventKit in the app, a fake in tests.
@MainActor
public protocol CapcomEventWriter: AnyObject {
    /// Whether this device can write at all. The Mac app built without the calendar
    /// entitlement cannot.
    var canWrite: Bool { get }
    /// Asks for write access at the first Add, then writes. Throws why it did not.
    func write(_ draft: CapcomEventDraft) async throws
}

/// The Add, Edit and Dismiss of calendar and reminder cards, from the thread and from the
/// push's Add action (capcom-service.md 8.5). One claim per card at a time.
@MainActor
@Observable
public final class CapcomCardAdder {
    public enum Outcome: Equatable, Sendable {
        /// The hab took the answer and the device wrote the event.
        case added
        case dismissed
        /// Another device answered first; nothing was written.
        case conflict(String)
        /// The hab refused or could not be reached; nothing was written.
        case refused(String)
        /// The hab took the answer, then the write failed.
        case writeFailed(String)
        /// Nothing happened: a claim on its way, no card, or no writer.
        case none
    }

    public typealias Claim = (CapcomCardAnswerParams) async throws -> CapcomCardAnswered

    @ObservationIgnored public var writer: CapcomEventWriter?
    /// This device's hello label, which the hab records as `answer.by`.
    @ObservationIgnored public var label: String
    /// Cards whose answer is on its way.
    public private(set) var busy: Set<String> = []
    /// Why the write failed after the hab took the answer, by item id.
    public private(set) var failures: [String: String] = [:]
    /// Why the hab refused the last answer, by item id.
    public private(set) var refusals: [String: String] = [:]
    /// The draft each failed write had, for Retry.
    @ObservationIgnored private var drafts: [String: CapcomEventDraft] = [:]
    @ObservationIgnored private let log = Logger(subsystem: "com.flightdeck.app", category: "capcom-cards")

    public init(writer: CapcomEventWriter?, label: String) {
        self.writer = writer
        self.label = label
    }

    /// Whether Add can run on this device.
    public var canWrite: Bool { writer?.canWrite == true }

    /// Add, or Save in the editor with `edited`: claim the card, then write. The answered item
    /// goes to `applied` (the thread) at once.
    @discardableResult
    public func add(_ item: CapcomItem, edited: CapcomEventDraft? = nil, claim: Claim,
                    applied: ((CapcomItem) -> Void)? = nil) async -> Outcome {
        guard let writer, writer.canWrite, item.answer == nil, !busy.contains(item.id),
              let draft = edited ?? CapcomEventDraft.of(item) else { return .none }
        busy.insert(item.id)
        defer { busy.remove(item.id) }
        refusals[item.id] = nil
        let r: CapcomCardAnswered
        do {
            r = try await claim(CapcomCardAnswerParams(choice: "add", item: item.id))
        } catch {
            let why = ChatModel.refusal(error)
            refusals[item.id] = why
            if let e = error as? RPCError, e.kind == .conflict { return .conflict(why) }
            return .refused(why)
        }
        applied?(r.item)
        return await write(item.id, draft, writer)
    }

    /// The push's Add: claim by item id, then write the event the answered card carries.
    public func addFromPush(item id: String, claim: Claim) async -> Outcome {
        guard let writer, writer.canWrite, !busy.contains(id) else { return .none }
        busy.insert(id)
        defer { busy.remove(id) }
        let r: CapcomCardAnswered
        do {
            r = try await claim(CapcomCardAnswerParams(choice: "add", item: id))
        } catch {
            let why = ChatModel.refusal(error)
            if let e = error as? RPCError, e.kind == .conflict { return .conflict(why) }
            return .refused(why)
        }
        guard let draft = CapcomEventDraft.of(r.item) else { return .writeFailed("the card has no event") }
        return await write(id, draft, writer)
    }

    public func dismiss(_ item: CapcomItem, claim: Claim, applied: ((CapcomItem) -> Void)? = nil) async -> Outcome {
        guard item.answer == nil, !busy.contains(item.id) else { return .none }
        busy.insert(item.id)
        defer { busy.remove(item.id) }
        refusals[item.id] = nil
        do {
            let r = try await claim(CapcomCardAnswerParams(choice: "dismiss", item: item.id))
            applied?(r.item)
            return .dismissed
        } catch {
            let why = ChatModel.refusal(error)
            refusals[item.id] = why
            if let e = error as? RPCError, e.kind == .conflict { return .conflict(why) }
            return .refused(why)
        }
    }

    /// Whether Retry shows: the write failed here, and this device's answer added the card.
    public func canRetry(_ item: CapcomItem) -> Bool {
        failures[item.id] != nil && item.answer?.choice == "add" && item.answer?.by == label
    }

    /// Writes again after a failed write, on this device only.
    @discardableResult
    public func retry(_ item: CapcomItem) async -> Outcome {
        guard canRetry(item), let writer, !busy.contains(item.id),
              let draft = drafts[item.id] ?? CapcomEventDraft.of(item) else { return .none }
        busy.insert(item.id)
        defer { busy.remove(item.id) }
        return await write(item.id, draft, writer)
    }

    private func write(_ id: String, _ draft: CapcomEventDraft, _ writer: CapcomEventWriter) async -> Outcome {
        do {
            try await writer.write(draft)
            failures[id] = nil
            drafts[id] = nil
            return .added
        } catch {
            let why = (error as? LocalizedError)?.errorDescription ?? String(describing: error)
            log.notice("card write failed")
            failures[id] = why
            drafts[id] = draft
            return .writeFailed(why)
        }
    }

    /// A button on an open card.
    public enum Button: String, Sendable {
        case add, edit, dismiss
    }

    /// The buttons an open card shows: none once answered; Dismiss alone once its event has
    /// ended, since Edit's save counts as Add.
    public nonisolated static func buttons(_ item: CapcomItem, now: Date) -> [Button] {
        guard item.answer == nil, let d = CapcomEventDraft.of(item) else { return [] }
        if d.isPast(now) { return [.dismiss] }
        return [.add, .edit, .dismiss]
    }

    /// The answered line: "Added on phone", "Dismissed".
    public nonisolated static func answered(_ a: CapcomAnswer) -> String {
        switch a.choice {
        case "add": return a.by.map { "Added on \($0)" } ?? "Added"
        case "dismiss": return "Dismissed"
        default: return a.choice.capitalized
        }
    }
}

/// A write that failed, in words.
public struct CapcomWriteError: LocalizedError, Sendable {
    public var message: String
    public init(_ message: String) { self.message = message }
    public var errorDescription: String? { message }
}
