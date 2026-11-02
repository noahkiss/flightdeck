import Foundation
import FlightdeckShared
import HabAPI
import Observation
import os
#if os(iOS)
import UIKit
#endif

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
    @ObservationIgnored private let now: () -> Date

    static let pendingKey = "flightdeck.approvals.reraise"

    public init(board: Board, answerer: ApprovalAnswerer, defaults: UserDefaults = .standard, now: @escaping () -> Date = Date.init) {
        self.board = board
        self.answerer = answerer
        self.defaults = defaults
        self.now = now
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

    /// Opens a `flightdeck://request` link (`RequestLink`). Returns false for
    /// any other link.
    @discardableResult
    public func open(url: URL) -> Bool {
        guard let m = RequestLink.meta(url) else { return false }
        open(m)
        return true
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

    // MARK: - The chat view's request card

    /// What the chat view's request card shows for one session (app.md 4.2).
    public struct CardState: Equatable, Sendable {
        public var request: OpenRequest
        /// Whether the card's answer buttons are on.
        public var canAnswer: Bool
        public var sending: Bool
        /// Why the last answer from the card did not land.
        public var failure: String?
        public var expired: Bool
    }

    /// The session's open request, as the card shows it; nil when none waits.
    /// It reads the sheet's state while the sheet holds that request, so a
    /// failure or a send in flight shows on the card too.
    public func card(for target: SessionTarget) -> CardState? {
        guard let pane = target.pane,
              let r = sheet.requests.first(where: { $0.habID == target.habID && $0.pane == pane })
        else { return nil }
        if sheet.current?.id == r.id {
            let phase = sheet.phase
            var failure: String?
            if case .failed(let why) = phase { failure = why }
            return CardState(request: r, canAnswer: sheet.canAnswer, sending: phase == .sending, failure: failure, expired: phase == .expired)
        }
        let expired = r.expiry.map { $0 <= now() } ?? false
        return CardState(request: r, canAnswer: r.answerable && !r.offline && !expired, sending: false, failure: nil, expired: expired)
    }

    /// A tap on the card: the approval sheet at the session's request.
    public func open(_ target: SessionTarget) {
        guard let r = card(for: target)?.request else { return }
        if sheet.show(id: r.id) { presenting = true }
    }

    /// Allow or Deny from the card. It answers through the sheet's path,
    /// so the rules are the same: one answer, the expiry, answered elsewhere.
    public func answer(_ target: SessionTarget, allow: Bool) async {
        // Never another session's request under this card's Allow.
        guard let r = card(for: target)?.request, sheet.show(id: r.id), sheet.current?.id == r.id else { return }
        if allow { await sheet.allow() } else { await sheet.deny() }
    }

    // MARK: - Answers from outside the app's windows

    /// Answers from a notification action or the Live Activity. A failure
    /// posts a notice and raises the request again in the foreground; a
    /// refusal says it was already answered or expired.
    ///
    /// A locked device signs once it has been unlocked after a restart
    /// (transport.md 7.2), so a Watch answer goes through: iOS mirrors a
    /// banner to the Watch only while the phone is locked (app.md 16.4).
    /// Two cases cannot sign and are refused at once, with nothing sent and
    /// the request raised again in the foreground: the device has not been
    /// unlocked since it started, or the hab's key is an earlier build's key
    /// that signs only while unlocked and the host has not taken the new
    /// one yet. Returns nil when it was refused that way.
    @discardableResult
    public func answer(host: String, _ params: ApprovalAnswerParams, meta: PushMeta?) async -> AnswerOutcome? {
        if !isUnlocked() {
            let state = lockedSigning(host)
            if state != .ready {
                log.notice("background answer \(params.choice.rawValue, privacy: .public): locked \(String(describing: state), privacy: .public)")
                let n = Self.lockedNotice(state, hab: habLabel(host: host))
                await post?(n.title, n.body, meta)
                if let meta { addPending(meta) }
                return nil
            }
        }
        let o = await answerer.answer(host: host, params)
        // The case is public; the hab's hint or the dial error may name hosts or commands.
        log.notice("background answer \(params.choice.rawValue, privacy: .public): \(o.caseName, privacy: .public) \(o.detail, privacy: .private)")
        let label = habLabel(host: host)
        if let f = o.followUp(hab: label, expiry: meta?.expiry) {
            await post?(f.title, f.body, o.reraise ? meta : nil)
        }
        if o.reraise, let meta {
            addPending(meta)
        }
        return o
    }

    /// Whether the device is unlocked (protected data available). The default
    /// reads UIKit on iOS, so it holds on a background launch for a
    /// notification action, before any scene exists; tests replace it.
    @ObservationIgnored public var isUnlocked: @MainActor () -> Bool = ApprovalCenter.protectedDataAvailable

    /// With the device locked, whether the key for this hab can sign
    /// (`DeviceKeys.lockedSigning`). The app sets it; the default refuses,
    /// as a key of an earlier build would.
    @ObservationIgnored public var lockedSigning: @MainActor (_ host: String) -> LockedSigning = { _ in .keyLocked }

    static func protectedDataAvailable() -> Bool {
        #if os(iOS)
        return UIApplication.shared.isProtectedDataAvailable
        #else
        return true
        #endif
    }

    /// The notice a locked device posts when it cannot sign. It names the
    /// device to unlock, since the Watch that shows it is already unlocked.
    static var lockedTitle: String {
        #if os(iOS)
        return "Unlock \(deviceName) to answer"
        #else
        return "Unlock to answer"
        #endif
    }

    /// The device's name in a notice: iPhone, iPad, or none on the Mac.
    static var deviceName: String {
        #if os(iOS)
        return UIDevice.current.userInterfaceIdiom == .pad ? "iPad" : "iPhone"
        #else
        return "this Mac"
        #endif
    }

    /// The title and body for a refused answer.
    static func lockedNotice(_ state: LockedSigning, hab: String) -> (title: String, body: String) {
        switch state {
        case .beforeFirstUnlock:
            return (lockedTitle, "\(deviceName) restarted and has not been unlocked yet · \(hab)")
        case .keyLocked, .ready:
            return (lockedTitle, "\(hab) does not have this device's new key yet")
        }
    }

    /// Allow or Deny from the Live Activity, with the unlock rule of `answer`.
    public func answerFromActivity(host: String, pane: String, request: String, allow: Bool) async -> AnswerOutcome? {
        let meta = PushMeta(kind: "permission", hab: host, pane: pane, request: request)
        return await answer(host: host, ApprovalAnswerParams(choice: allow ? .allow : .deny, pane: pane, request: request, via: .banner), meta: meta)
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
