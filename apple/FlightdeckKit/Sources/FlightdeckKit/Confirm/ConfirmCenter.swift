import Foundation
import HabAPI
import Observation

/// One confirmation card (capcom.md 6.3): the confirmation, the hab it lives
/// on, and the requesting session found by name on that hab.
public struct ConfirmCard: Identifiable, Equatable, Sendable {
    public var habID: String
    public var habLabel: String
    public var confirmation: Confirmation
    /// The requesting session; nil for a request from outside a pane, or a
    /// session this hab no longer lists.
    public var session: SessionTarget?
    /// The request came from Capcom: the card shows in the Capcom view.
    public var capcom: Bool
    /// The hab is not online: the card shows, its buttons are off.
    public var offline: Bool
    /// The hab's clock less this device's (app.md 14.6), for the countdown.
    public var clockOffset: TimeInterval?

    public var id: String { habID + "/" + confirmation.id }

    /// `expires_at` on this device's clock.
    public var deviceExpiry: Date? {
        confirmation.expiry.map { $0.addingTimeInterval(-(clockOffset ?? 0)) }
    }
}

/// The owner's changes on a start card. Every field starts at the card's value.
/// An empty field keeps the card's value: the hab never clears one (capcom.md 6.4).
public struct ConfirmDraft: Equatable, Sendable {
    public var host: String
    public var folder: String
    public var harness: String
    public var model: String
    public var seed: String

    public init(_ s: ConfirmStart) {
        host = s.host
        folder = s.folder
        harness = s.harness
        model = s.model ?? ""
        seed = s.seed ?? ""
    }

    /// The harnesses a start can run (capcom.md 6.4).
    public static let harnesses = ["claude", "codex", "opencode"]
    /// The longest seed the hab takes.
    public static let seedLimit = 8000

    /// What changed against the card, as `confirm.answer` takes it; nil when
    /// nothing did. A new host always carries its folder.
    public func edits(against s: ConfirmStart) -> ConfirmEdits? {
        func changed(_ v: String, _ old: String?) -> String? {
            let t = v.trimmingCharacters(in: .whitespacesAndNewlines)
            return t.isEmpty || t == (old ?? "") ? nil : t
        }
        var e = ConfirmEdits()
        e.host = changed(host, s.host)
        e.folder = changed(folder, s.folder)
        if e.host != nil, e.folder == nil {
            let f = folder.trimmingCharacters(in: .whitespacesAndNewlines)
            e.folder = f.isEmpty ? nil : f
        }
        e.harness = changed(harness, s.harness)
        e.model = changed(model, s.model)
        // The seed keeps its own spacing.
        e.seed = seed.isEmpty || seed == (s.seed ?? "") ? nil : seed
        return e == ConfirmEdits() ? nil : e
    }

    /// The start as it will run with this draft. A new host or folder drops
    /// the project and branch, which the hab fills in again.
    public func applied(to s: ConfirmStart) -> ConfirmStart {
        guard let e = edits(against: s) else { return s }
        var out = s
        if let v = e.host { out.host = v }
        if let v = e.folder { out.folder = v }
        if e.host != nil || e.folder != nil {
            out.project = nil
            out.branch = nil
        }
        if let v = e.harness { out.harness = v }
        if let v = e.model { out.model = v }
        if let v = e.seed { out.seed = v }
        return out
    }

    /// Why Confirm is off for this draft; nil when it can go.
    public func problem(against s: ConfirmStart) -> String? {
        // The hab counts runes (Unicode scalars), not grapheme clusters.
        if seed.unicodeScalars.count > Self.seedLimit { return "The seed is longer than \(Self.seedLimit) characters" }
        if let e = edits(against: s), e.host != nil, e.folder == nil { return "Give the folder on the new host" }
        return nil
    }
}

/// Confirmation cards across every hab (capcom.md 6.1, 6.3; app.md 7):
/// which cards show where, the owner's edits, the answers and the countdown.
///
/// - Capcom's cards show in the Capcom view; another agent's in its session
///   view and in the approvals area. A card from outside a pane shows in the
///   approvals area only.
/// - The approvals area shows questions, and starts this client confirmed that
///   failed or ended unknown, until dismissed. Records and other closed cards
///   stay in their session view (and the Capcom view), with their outcome.
@MainActor
@Observable
public final class ConfirmCenter {
    public enum Phase: Equatable, Sendable {
        /// Confirm and Cancel on.
        case open
        case sending
        /// The answer did not land; the buttons stay on to try again.
        case failed(String)
        /// Another client answered first (`conflict`).
        case alreadyAnswered
        /// Past `expires_at`: nothing runs.
        case expired
        /// The hab is not online.
        case offline
        /// Confirmed; the hab runs it.
        case running
        /// It ran at once, with no question.
        case record
        /// Closed: done, failed, cancelled, expired or unknown.
        case closed
    }

    public typealias Answer = @MainActor (_ habID: String, ConfirmAnswerParams) async throws -> ConfirmationResult
    public typealias List = @MainActor (_ habID: String) async throws -> ConfirmList

    @ObservationIgnored private let board: Board
    @ObservationIgnored private let answerer: Answer
    @ObservationIgnored private let lister: List?
    @ObservationIgnored private let now: () -> Date

    /// Marks more sessions as Capcom, by hab and name, beside the sessions the
    /// hab files with kind `capcom` (capcom.md 7.2). The default adds none.
    @ObservationIgnored public var isCapcom: @MainActor (_ habID: String, _ session: String) -> Bool = { _, _ in false }

    private var drafts: [String: ConfirmDraft] = [:]
    private var sending: Set<String> = []
    private var failures: [String: String] = [:]
    private var conflicts: Set<String> = []
    /// Starts this client confirmed that failed or ended unknown, by card id,
    /// as the answer or the closed event last had them: the approvals area
    /// keeps them until dismissed.
    private var held: [String: ConfirmCard] = [:]
    /// Cards this client sent a confirm for, by card id. A failed or unknown
    /// close holds the card even when the answer never came back.
    private var confirmed: Set<String> = []
    /// Held cards the owner dismissed: never held again.
    private var dismissed: Set<String> = []
    /// The hosts a start can go to, per hab, from `confirm.list`.
    public private(set) var targets: [String: [ConfirmTarget]] = [:]

    public init(board: Board, now: @escaping () -> Date = Date.init, answer: @escaping Answer, list: List? = nil) {
        self.board = board
        self.answerer = answer
        self.lister = list
        self.now = now
    }

    // MARK: - Cards

    /// Every card, open ones first in arrival order, then closed ones newest first.
    public var cards: [ConfirmCard] {
        var open: [ConfirmCard] = []
        var closed: [ConfirmCard] = []
        for h in board.habs {
            let book = h.state.confirmations
            guard book.enabled else { continue }
            let offline = h.state.status != .online
            func card(_ c: Confirmation) -> ConfirmCard {
                let s = c.from.flatMap { name in h.state.store.sessions.values.first { $0.name == name } }
                return ConfirmCard(
                    habID: h.id, habLabel: h.label, confirmation: c,
                    session: s.map { SessionTarget(habID: h.id, session: $0.id, pane: $0.pane) },
                    capcom: s?.kind == .capcom || (c.from.map { isCapcom(h.id, $0) } ?? false),
                    offline: offline, clockOffset: h.state.clockOffset)
            }
            open += book.open.map(card)
            closed += book.closed.map(card)
        }
        open.sort { Self.time($0.confirmation.created) < Self.time($1.confirmation.created) }
        closed.sort { Self.time($0.confirmation.closed) > Self.time($1.confirmation.closed) }
        return open + closed
    }

    /// An RFC 3339 stamp as a date; distant past when absent or unreadable.
    nonisolated static func time(_ s: String?) -> Date {
        s.flatMap(UsageBook.parseTime) ?? .distantPast
    }

    /// The approvals area: questions from agents other than Capcom, oldest
    /// first, then failed or unknown starts this client confirmed, newest first.
    public var approvals: [ConfirmCard] {
        let all = cards
        let asks = all.filter { $0.confirmation.asks && !$0.capcom }
        let kept = held.values.map { h in all.first { $0.id == h.id && $0.confirmation.isClosed } ?? h }
            .sorted { Self.time($0.confirmation.closed) > Self.time($1.confirmation.closed) }
        return asks + kept
    }

    /// Whether the approvals area keeps this card until it is dismissed.
    public func isHeld(_ card: ConfirmCard) -> Bool { held[card.id] != nil }

    /// Takes a failed or unknown start out of the approvals area.
    public func dismiss(_ card: ConfirmCard) {
        held[card.id] = nil
        dismissed.insert(card.id)
    }

    /// Whether this client answered the card: it sent a confirm for it, or the
    /// hab names this client's id as the one that answered.
    private func answeredHere(_ card: ConfirmCard) -> Bool {
        if confirmed.contains(card.id) { return true }
        guard let by = card.confirmation.answeredBy, !by.isEmpty else { return false }
        return board.habs.first { $0.id == card.habID }?.state.welcome?.clientId == by
    }

    /// Drops the edits, notes and marks of cards that are no longer open on
    /// their hab. A held card stays until dismissed.
    ///
    /// A start this client answered that closed failed or unknown is held too
    /// when the answer itself never returned: the closed event, or the
    /// `confirm.list` after a reconnect, brings the outcome.
    public func boardChanged() {
        let open = Set(cards.filter { !$0.confirmation.isClosed }.map(\.id))
        let all = cards
        for (id, h) in held {
            // A newer copy (the closed event after the answer) replaces the held one.
            if let c = all.first(where: { $0.id == id && $0.confirmation.isClosed }), c != h { held[id] = c }
        }
        for c in all where held[c.id] == nil && !dismissed.contains(c.id) {
            let s = c.confirmation.state
            if s == .failed || s == .unknown, c.confirmation.record != true, answeredHere(c) { held[c.id] = c }
        }
        // Answer marks go with their card, but not while a reconnect waits for
        // `confirm.list` to say how a card held open before it ended.
        var known = Set(all.map(\.id))
        for h in board.habs { known.formUnion(h.state.confirmations.pending.map { h.id + "/" + $0 }) }
        if confirmed.contains(where: { !known.contains($0) }) { confirmed = confirmed.filter(known.contains) }
        if dismissed.contains(where: { !known.contains($0) }) { dismissed = dismissed.filter(known.contains) }
        let keep = { (id: String) in open.contains(id) || self.sending.contains(id) }
        if drafts.keys.contains(where: { !keep($0) }) { drafts = drafts.filter { keep($0.key) } }
        if failures.keys.contains(where: { !keep($0) }) { failures = failures.filter { keep($0.key) } }
        if conflicts.contains(where: { !keep($0) }) { conflicts = conflicts.filter(keep) }
    }

    /// The Capcom view's cards.
    public var capcom: [ConfirmCard] {
        cards.filter(\.capcom)
    }

    /// One session view's cards: its open ones, then at most `closedShown` closed.
    public func cards(for target: SessionTarget) -> [ConfirmCard] {
        let mine = cards.filter { $0.session?.habID == target.habID && $0.session?.session == target.session && target.session != nil }
        return mine.filter { !$0.confirmation.isClosed } + mine.filter(\.confirmation.isClosed).prefix(Self.closedShown)
    }

    /// Closed cards a session view keeps under its open ones.
    nonisolated public static let closedShown = 3

    /// Questions waiting across every hab.
    public var waitingCount: Int { cards.filter { $0.confirmation.asks }.count }

    // MARK: - State of one card

    public func phase(_ card: ConfirmCard) -> Phase {
        let c = card.confirmation
        if c.record == true, !c.isClosed { return .record }
        if c.isClosed { return .closed }
        if c.state == .running { return .running }
        if conflicts.contains(card.id) { return .alreadyAnswered }
        if sending.contains(card.id) { return .sending }
        if let e = card.deviceExpiry, e <= now() { return .expired }
        if card.offline { return .offline }
        if let f = failures[card.id] { return .failed(f) }
        return .open
    }

    /// Whether Confirm and Cancel are on.
    public func canAnswer(_ card: ConfirmCard) -> Bool {
        guard card.confirmation.asks else { return false }
        switch phase(card) {
        case .open, .failed: return true
        default: return false
        }
    }

    /// Whether this card shows Confirm and Cancel at all (off or on).
    public func hasButtons(_ card: ConfirmCard) -> Bool {
        card.confirmation.record != true && !card.confirmation.isClosed && card.confirmation.state != .running
    }

    /// Seconds left before the card expires, at `date`; nil with no expiry or
    /// once it is not a question.
    public func remaining(_ card: ConfirmCard, at date: Date? = nil) -> TimeInterval? {
        guard card.confirmation.asks, let e = card.deviceExpiry else { return nil }
        return max(0, e.timeIntervalSince(date ?? now()))
    }

    /// "4m 12s", "1h 02m", "12s".
    nonisolated public static func countdown(_ t: TimeInterval) -> String {
        let s = Int(t.rounded(.up))
        if s >= 3600 { return String(format: "%dh %02dm", s / 3600, (s % 3600) / 60) }
        if s >= 60 { return "\(s / 60)m \(String(format: "%02d", s % 60))s" }
        return "\(s)s"
    }

    // MARK: - Edits

    public func draft(_ card: ConfirmCard) -> ConfirmDraft? {
        guard let s = card.confirmation.start else { return nil }
        return drafts[card.id] ?? ConfirmDraft(s)
    }

    public func setDraft(_ d: ConfirmDraft, for card: ConfirmCard) {
        drafts[card.id] = d
        failures[card.id] = nil
    }

    /// Drops the owner's changes.
    public func resetDraft(_ card: ConfirmCard) {
        drafts[card.id] = nil
    }

    /// Whether the owner changed anything on the card.
    public func isEdited(_ card: ConfirmCard) -> Bool {
        guard let s = card.confirmation.start, let d = drafts[card.id] else { return false }
        return d.edits(against: s) != nil
    }

    /// Asks the card's hab which hosts a start can go to.
    public func loadTargets(_ habID: String) async {
        guard let lister, let r = try? await lister(habID) else { return }
        targets[habID] = r.targets ?? []
    }

    // MARK: - Answers

    public func confirm(_ card: ConfirmCard) async {
        var edits: ConfirmEdits?
        if let s = card.confirmation.start, let d = drafts[card.id] {
            if let p = d.problem(against: s) {
                failures[card.id] = p
                return
            }
            edits = d.edits(against: s)
        }
        await answer(card, ConfirmAnswerParams(choice: .confirm, edits: edits, id: card.confirmation.id))
    }

    public func cancel(_ card: ConfirmCard) async {
        await answer(card, ConfirmAnswerParams(choice: .cancel, id: card.confirmation.id))
    }

    private func answer(_ card: ConfirmCard, _ p: ConfirmAnswerParams) async {
        guard canAnswer(card) else { return }
        sending.insert(card.id)
        failures[card.id] = nil
        defer { sending.remove(card.id) }
        // An earlier confirm whose answer never came back may still have run.
        let earlier = confirmed.contains(card.id)
        if p.choice == .confirm { confirmed.insert(card.id) }
        func refused() { if !earlier { confirmed.remove(card.id) } }
        do {
            let r = try await answerer(card.habID, p)
            drafts[card.id] = nil
            if p.choice == .confirm, r.confirmation.state == .failed || r.confirmation.state == .unknown {
                var c = card
                c.confirmation = r.confirmation
                held[card.id] = c
            }
        } catch let e as RPCError where e.kind == .conflict {
            refused()
            conflicts.insert(card.id)
        } catch let e as RPCError {
            refused()
            failures[card.id] = e.data?.hint ?? e.message
        } catch {
            // No answer (a dropped link, the time limit): the confirm may still run.
            failures[card.id] = String(describing: error)
        }
    }

    /// The text that says how a closed card ended.
    nonisolated public static func outcome(_ c: Confirmation) -> String {
        switch c.state {
        case .done:
            let n = c.result?.name ?? c.start?.name ?? "the session"
            return "Started \(n) on \(c.result?.host ?? c.start?.host ?? "")"
        case .failed: return "Failed" + (c.reason.map { ": \($0)" } ?? "")
        case .cancelled: return "Cancelled"
        case .expired: return "Expired: nothing ran"
        case .unknown: return "Outcome unknown" + (c.reason.map { ": \($0)" } ?? "")
        case .running: return "Running"
        default: return c.state.rawValue
        }
    }
}
