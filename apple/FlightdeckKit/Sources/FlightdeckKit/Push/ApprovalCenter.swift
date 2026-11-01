import Foundation
import FlightdeckShared
import HabAPI
import Observation
import os

/// Where approvals meet the app: the sheet and when it shows, the answers from
/// notifications and the Live Activity, and the foreground prompt that a
/// failed background answer raises again (app.md 7.3, 7.8, 18.7).
@MainActor
@Observable
public final class ApprovalCenter {
    public let sheet: ApprovalSheetModel
    /// The view shows the sheet while this is true.
    public var presenting = false

    @ObservationIgnored private let board: Board
    @ObservationIgnored private let answerer: ApprovalAnswerer
    @ObservationIgnored private let defaults: UserDefaults
    @ObservationIgnored private let log = Logger(subsystem: "com.flightdeck.app", category: "approvals")
    /// Posts a local notification (title, body, meta); the app sets it.
    @ObservationIgnored public var post: (@MainActor (String, String, PushMeta?) async -> Void)?
    /// Removes delivered notifications of requests that closed.
    @ObservationIgnored public var withdraw: (@MainActor (_ requests: Set<String>) async -> Void)?
    @ObservationIgnored private var lastOpen: Set<String> = []

    static let pendingKey = "flightdeck.approvals.reraise"

    public init(board: Board, answerer: ApprovalAnswerer, defaults: UserDefaults = .standard, now: @escaping () -> Date = Date.init) {
        self.board = board
        self.answerer = answerer
        self.defaults = defaults
        let a = answerer
        sheet = ApprovalSheetModel(board: board, now: now) { req, params in
            guard let host = req.host else { return .unreachable("the hab has not said its host id yet") }
            return await a.answer(host: host, params)
        }
    }

    // MARK: - Opening the sheet

    /// The owner tapped a rank 1 row (app.md 7.2).
    public func open(_ row: BoardRow) {
        if sheet.show(habID: row.target.habID, pane: row.target.pane) { presenting = true }
    }

    /// The owner tapped a notification, or the Live Activity. A request whose
    /// hab is not online yet opens once its snapshot arrives.
    public func open(_ meta: PushMeta) {
        guard meta.kind == "permission" || meta.kind == "question" || meta.request != nil else { return }
        if sheet.show(host: meta.hab, pane: meta.pane, request: meta.request) {
            presenting = true
        } else {
            addPending(meta)
        }
    }

    /// The board changed: show a pending prompt whose request is now open,
    /// drop ones whose hab is online and no longer has the request, and
    /// withdraw delivered banners of requests that closed.
    public func boardChanged() {
        let open = Set(OpenRequest.collect(board.habs).compactMap(\.requestID))
        let closed = lastOpen.subtracting(open)
        lastOpen = open
        if !closed.isEmpty, let withdraw {
            Task { await withdraw(closed) }
        }
        var keep: [PushMeta] = []
        for m in pending {
            let habOnline = board.habs.contains { ($0.state.welcome?.host.id ?? $0.state.store.host) == m.hab && $0.state.status == .online }
            if let r = m.request, open.contains(r) {
                if !presenting, sheet.show(host: m.hab, pane: m.pane, request: m.request) { presenting = true }
                continue
            }
            if !habOnline { keep.append(m) }
        }
        pending = keep
    }

    // MARK: - Answers from outside the app's windows

    /// Answers from a notification action or the Live Activity. A failure
    /// posts a notice and raises the request again in the foreground; a
    /// refusal says it was already answered or expired.
    public func answer(host: String, _ params: ApprovalAnswerParams, meta: PushMeta?) async -> AnswerOutcome {
        let o = await answerer.answer(host: host, params)
        log.notice("background answer \(params.choice.rawValue, privacy: .public): \(String(describing: o), privacy: .public)")
        let label = habLabel(host: host)
        if let f = o.followUp(hab: label, expiry: meta?.expiry) {
            await post?(f.title, f.body, o.reraise ? meta : nil)
        }
        if o.reraise, let meta {
            addPending(meta)
        }
        return o
    }

    func habLabel(host: String) -> String {
        board.habs.first { ($0.state.welcome?.host.id ?? $0.state.store.host) == host }?.label ?? host
    }

    // MARK: - Pending prompts

    /// Requests to raise again when the app is next in front.
    public private(set) var pending: [PushMeta] {
        get {
            guard let d = defaults.data(forKey: Self.pendingKey) else { return [] }
            return (try? JSONDecoder().decode([PushMeta].self, from: d)) ?? []
        }
        set {
            if newValue.isEmpty {
                defaults.removeObject(forKey: Self.pendingKey)
            } else if let d = try? JSONEncoder().encode(Array(newValue.suffix(16))) {
                defaults.set(d, forKey: Self.pendingKey)
            }
        }
    }

    private func addPending(_ m: PushMeta) {
        var p = pending
        p.removeAll { $0.hab == m.hab && $0.pane == m.pane && $0.request == m.request }
        p.append(m)
        pending = p
    }
}
