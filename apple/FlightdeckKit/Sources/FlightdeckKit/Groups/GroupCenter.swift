import Foundation
import HabAPI
import Observation

/// The group record (groups.md 5.1), named apart from SwiftUI's `Group`.
public typealias GroupRecord = Group

/// Applies `group.updated` (groups.md 15.1) to a hab's state.
enum GroupEvents {
    static let updated = "group.updated"

    /// A record whose revision is not above the held one is ignored, as for any object within
    /// an epoch (hab-logic-api.md 3.5); a new epoch brings a new snapshot first. Returns
    /// whether the state changed.
    static func apply(_ e: Event, to state: inout HabState) -> Bool {
        guard e.type == updated, var g = try? e.data.decode(Group.self) else { return false }
        let rev = e.revision ?? g.revision
        if let held = state.groups[g.id], rev <= held.revision { return false }
        g.revision = rev
        state.groups[g.id] = g
        return true
    }
}

/// One group's card (groups.md 5.4): the record and the home hab that holds it.
public struct GroupCard: Identifiable, Hashable, Sendable {
    public var habID: String
    public var habLabel: String
    public var group: Group
    /// The home hab is online: the actions reach it.
    public var online: Bool

    public var id: String { "\(habID)/g/\(group.id)" }

    /// Ended or dropped: the card shows the outcome and offers no action.
    public var isOver: Bool { group.state == "ended" || group.state == "dropped" }
    /// Pause holds a running group, or adds the user's reason to a usage pause.
    public var canPause: Bool { online && (group.state == "running" || group.state == "paused") && group.pause.user == nil }
    /// Resume lifts the user's pause; a usage pause stays (groups.md 5.6).
    public var canResume: Bool { online && group.state == "paused" && group.pause.user != nil }
    /// Close ends a running or paused group; the hab cleans up (groups.md 7.5).
    public var canClose: Bool { online && (group.state == "running" || group.state == "paused") }

    /// Start again: a failed or closed member, or one whose setup waited, while the group runs
    /// or is paused (groups.md 4.3).
    public func canRetry(_ m: GroupMember) -> Bool {
        guard online, group.state == "running" || group.state == "paused" else { return false }
        if m.state == "failed" || m.state == "exited" { return true }
        return m.state == "planned" && (group.items.first { $0.id == m.item }?.reason.map { !$0.isEmpty } ?? false)
    }
}

/// The groups of every hab in the app (groups.md 5.4): the cards on the status board and in
/// the coordinator's session view, the member rows that open a session, and the actions.
@MainActor
@Observable
public final class GroupCenter {
    /// The welcome capability that says a hab runs groups.
    public nonisolated static let capability = "groups"
    /// How long an ended or dropped group stays on the status board. The coordinator's
    /// session view keeps every group it made.
    public nonisolated static let overShown: TimeInterval = 24 * 3600

    /// The last refusal or failure of an action, by card id.
    public private(set) var errors: [String: String] = [:]
    /// Actions in flight, by card id.
    public private(set) var busy: Set<String> = []
    /// A Close waiting for the owner's yes.
    public var closeAsk: GroupCard?
    /// The member session a card asked to open; the board opens it and clears this.
    public var openRequest: SessionTarget?
    /// What the Close question says.
    public let closeMessage = "Nothing new starts. Idle members close when \"Close idle members at the end\" is on."

    @ObservationIgnored private let board: Board
    @ObservationIgnored private let connection: @MainActor (String) -> HabConnection?
    @ObservationIgnored private let device: @MainActor (String) -> String?
    /// The clock the board's cut-off is timed by; tests move it.
    @ObservationIgnored var now: @MainActor () -> Date = { Date() }

    /// `device` finds the hab in the app with a device id.
    public init(board: Board, connection: @escaping @MainActor (String) -> HabConnection?,
                device: @escaping @MainActor (String) -> String? = { _ in nil }) {
        self.board = board
        self.connection = connection
        self.device = device
    }

    // MARK: - Cards

    /// Every group of every hab whose welcome lists `groups`. An offline hab keeps the groups
    /// it last showed.
    public var all: [GroupCard] {
        var out: [GroupCard] = []
        for h in board.habs where h.state.welcome?.capabilities.contains(Self.capability) == true {
            for g in h.state.groups.values {
                out.append(GroupCard(habID: h.id, habLabel: h.label, group: g, online: h.state.status == .online))
            }
        }
        return Self.order(out)
    }

    /// The status board's cards: live groups, and those that ended within `overShown`.
    public var boardCards: [GroupCard] {
        let t = now()
        return all.filter { c in
            guard c.isOver else { return true }
            let end = Self.parse(c.group.ended ?? c.group.updated) ?? .distantPast
            return t.timeIntervalSince(end) < Self.overShown
        }
    }

    /// The groups a coordinator made, for its session view: the groups on its hab whose
    /// coordinator is that session.
    public func cards(coordinator t: SessionTarget) -> [GroupCard] {
        guard let s = t.session else { return [] }
        return all.filter { $0.habID == t.habID && $0.group.coordinator.session == s }
    }

    public func card(_ id: String) -> GroupCard? { all.first { $0.id == id } }

    /// Live groups first (confirming, running, paused, ending), then ended and dropped;
    /// newest first inside each.
    public nonisolated static func order(_ cards: [GroupCard]) -> [GroupCard] {
        func rank(_ s: String) -> Int {
            switch s {
            case "confirming": return 0
            case "running": return 1
            case "paused": return 2
            case "ending": return 3
            case "ended", "dropped": return 5
            default: return 4
            }
        }
        return cards.sorted { a, b in
            let ra = rank(a.group.state), rb = rank(b.group.state)
            if ra != rb { return ra < rb }
            if a.group.created != b.group.created { return a.group.created > b.group.created }
            return a.id < b.id
        }
    }

    /// The session a member row opens: the member's session on its hab, found by device id,
    /// else the home hab. Nil before the member started, or when no hab in the app holds it.
    public func target(_ card: GroupCard, _ m: GroupMember) -> SessionTarget? {
        guard let s = m.session, !s.isEmpty else { return nil }
        let habID = m.device.flatMap(device) ?? (m.device == nil ? card.habID : nil)
        guard let habID, let h = board.habs.first(where: { $0.id == habID }) else { return nil }
        return SessionTarget(habID: habID, session: s, pane: h.state.store.sessions[s]?.pane)
    }

    /// Opens a member's session (groups.md 5.4). Returns false when it has none yet.
    @discardableResult
    public func open(_ card: GroupCard, _ m: GroupMember) -> Bool {
        guard let t = target(card, m) else { return false }
        openRequest = t
        return true
    }

    // MARK: - Actions

    public func pause(_ card: GroupCard) async {
        await act(card) { conn in _ = try await conn.call(HabMethods.groupPause, GroupRef(group: card.group.id)) }
    }

    public func resume(_ card: GroupCard) async {
        await act(card) { conn in _ = try await conn.call(HabMethods.groupResume, GroupRef(group: card.group.id)) }
    }

    /// Close asks first: this only holds the question.
    public func askClose(_ card: GroupCard) { closeAsk = card }

    /// The owner said yes to the held Close. It takes the held card at once and starts the
    /// call: the dialog clears `closeAsk` as it closes, before any task it starts can run.
    @discardableResult
    public func confirmClose() -> Task<Void, Never>? {
        guard let card = closeAsk else { return nil }
        closeAsk = nil
        return Task { await close(card) }
    }

    public func close(_ card: GroupCard) async {
        await act(card) { conn in _ = try await conn.call(HabMethods.groupClose, GroupRef(group: card.group.id)) }
    }

    /// Starts a failed or closed member again (`group.retry`).
    public func retry(_ card: GroupCard, _ m: GroupMember) async {
        await act(card) { conn in
            _ = try await conn.call(HabMethods.groupRetry, GroupRetryParams(group: card.group.id, member: m.name))
        }
    }

    public func clearError(_ card: GroupCard) { errors[card.id] = nil }

    private func act(_ card: GroupCard, _ work: @MainActor (HabConnection) async throws -> Void) async {
        errors[card.id] = nil
        guard let conn = connection(card.habID) else {
            errors[card.id] = "That hab is no longer in the app."
            return
        }
        busy.insert(card.id)
        defer { busy.remove(card.id) }
        do {
            try await work(conn)
        } catch let e as RPCError {
            errors[card.id] = e.data?.hint ?? e.message
        } catch {
            errors[card.id] = String(describing: error)
        }
    }

    // MARK: - Text

    nonisolated static func parse(_ s: String?) -> Date? {
        guard let s else { return nil }
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }
}

/// The card's words (groups.md 5.4).
public enum GroupText {
    /// The time from creation to the end, or to now.
    public static func elapsed(_ g: Group, now: Date) -> String? {
        guard let start = GroupCenter.parse(g.created) else { return nil }
        let end = g.ended.flatMap(GroupCenter.parse) ?? now
        let s = max(0, Int(end.timeIntervalSince(start)))
        switch s {
        case ..<60: return "\(s)s"
        case ..<3600: return "\(s / 60)m"
        case ..<(48 * 3600): return "\(s / 3600)h \((s % 3600) / 60)m"
        default: return "\(s / 86400)d"
        }
    }

    /// "waiting: disk on beta, …", from the record's waits; nil when nothing holds it.
    public static func waits(_ g: Group) -> String? {
        g.waits.isEmpty ? nil : "waiting: " + g.waits.joined(separator: ", ")
    }

    /// Why a paused group holds: the user, usage, or both.
    public static func pause(_ g: Group) -> String? {
        switch (g.pause.user != nil, g.pause.usage != nil) {
        case (true, true): return "Paused by you and by usage"
        case (true, false): return "Paused by you"
        case (false, true): return "Paused by usage" + (g.pause.until.map { " until \($0)" } ?? "")
        default: return nil
        }
    }

    /// The header line's counts: "2 of 3 landed · 4 members".
    public static func counts(_ g: Group) -> String {
        let landed = g.items.filter { $0.state == "landed" }.count
        let live = g.items.filter { $0.state != "dropped" }.count
        return "\(landed) of \(live) landed · \(g.members.count) member\(g.members.count == 1 ? "" : "s")"
    }

    /// The member's state as a session row shows it: the agent state once started, else the
    /// start's state.
    public static func memberState(_ m: GroupMember) -> String {
        if m.state == "running", let a = m.agent?.rawValue, !a.isEmpty { return a }
        return m.state
    }

    /// The lane line: what lands now and what waits, by item title.
    public static func lane(_ g: Group) -> String? {
        func title(_ id: String) -> String { g.items.first { $0.id == id }?.title ?? id }
        var parts: [String] = []
        if let l = g.lane.landing, !l.isEmpty { parts.append("landing \(title(l))") }
        if !g.lane.queue.isEmpty { parts.append("queue: " + g.lane.queue.map(title).joined(separator: ", ")) }
        return parts.isEmpty ? nil : parts.joined(separator: " · ")
    }
}
