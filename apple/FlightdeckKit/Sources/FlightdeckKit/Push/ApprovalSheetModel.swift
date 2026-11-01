import Foundation
import HabAPI
import Observation

/// The approval sheet's state (app.md 7.2, 7.4, 7.5, 7.8). It holds one
/// request at a time, oldest first, and reads every request from the board,
/// so an answer on another device (the hab's `agent.state` event) turns the
/// buttons off at once and says the request was answered elsewhere.
@MainActor
@Observable
public final class ApprovalSheetModel {
    public enum Phase: Equatable, Sendable {
        /// Buttons on.
        case open
        case sending
        /// The answer did not land; the buttons stay on to try again.
        case failed(String)
        /// The request closed without this sheet's answer.
        case answeredElsewhere
        /// Past `expires_at`: answer in the terminal.
        case expired
    }

    public typealias Answer = @MainActor (OpenRequest, ApprovalAnswerParams) async -> AnswerOutcome

    private let board: Board
    private let answerer: Answer
    private let now: () -> Date

    /// The request on screen. It stays after the request closes, so the sheet
    /// can say where it went, until the owner moves on.
    public private(set) var current: OpenRequest?
    private var sending = false
    private var failure: String?
    /// Requests this sheet answered; they never show as "answered elsewhere".
    private var answeredHere: Set<String> = []
    /// Set when the last request is done: the view closes the sheet.
    public private(set) var finished = false

    public init(board: Board, now: @escaping () -> Date = Date.init, answer: @escaping Answer) {
        self.board = board
        self.answerer = answer
        self.now = now
    }

    /// Every open request on the board, oldest first.
    public var requests: [OpenRequest] { OpenRequest.collect(board.habs) }

    /// "1 of 3" (app.md 7.8), counting the request on screen.
    public var position: (index: Int, count: Int) {
        let rs = requests
        guard let c = current else { return (0, rs.count) }
        if let i = rs.firstIndex(where: { $0.id == c.id }) { return (i + 1, rs.count) }
        return (1, rs.count + 1)
    }

    /// The request on screen as the board has it now; nil once it closed.
    public var live: OpenRequest? {
        guard let c = current else { return nil }
        return requests.first { $0.id == c.id }
    }

    public var phase: Phase {
        guard let c = current else { return .answeredElsewhere }
        if sending { return .sending }
        if live == nil { return answeredHere.contains(c.id) ? .sending : .answeredElsewhere }
        if let e = c.expiry, e <= now() { return .expired }
        if let f = failure { return .failed(f) }
        return .open
    }

    /// Whether Allow and Deny are on.
    public var canAnswer: Bool {
        guard let c = live, c.answerable, !c.offline else { return false }
        switch phase {
        case .open, .failed: return true
        default: return false
        }
    }

    /// Seconds left before the request expires; nil with no deadline (7.4).
    public var remaining: TimeInterval? {
        guard let e = current?.expiry else { return nil }
        return max(0, e.timeIntervalSince(now()))
    }

    /// Opens the sheet at a request: by id, else the session's request, else
    /// the oldest. Returns false when nothing waits.
    @discardableResult
    public func show(id: String? = nil, habID: String? = nil, pane: String? = nil, request: String? = nil) -> Bool {
        let rs = requests
        finished = false
        failure = nil
        sending = false
        if let id, let r = rs.first(where: { $0.id == id }) {
            current = r
        } else if let pane, let r = rs.first(where: { ($0.habID == habID || habID == nil) && $0.pane == pane && (request == nil || $0.requestID == request) }) {
            current = r
        } else if let pane, let r = rs.first(where: { ($0.habID == habID || habID == nil) && $0.pane == pane }) {
            current = r
        } else {
            current = rs.first
        }
        return current != nil
    }

    /// Opens the sheet at the request a push named, found by host id.
    @discardableResult
    public func show(host: String, pane: String?, request: String?) -> Bool {
        let habID = board.habs.first { ($0.state.welcome?.host.id ?? $0.state.store.host) == host }?.id
        return show(habID: habID, pane: pane, request: request)
    }

    /// Moves to the next open request, or finishes when none is left.
    public func next() {
        failure = nil
        sending = false
        let rs = requests.filter { !answeredHere.contains($0.id) && $0.id != current?.id }
        if let r = rs.first {
            current = r
        } else {
            current = nil
            finished = true
        }
    }

    public func allow() async { await answer(.allow, text: nil) }
    public func deny() async { await answer(.deny, text: nil) }

    /// Answers a question with free text or the chosen options.
    public func reply(_ text: String) async { await answer(.answer, text: text) }

    private func answer(_ choice: ApprovalChoice, text: String?) async {
        guard let c = live, let rid = c.requestID, canAnswer else { return }
        sending = true
        failure = nil
        answeredHere.insert(c.id)
        let o = await answerer(c, ApprovalAnswerParams(choice: choice, pane: c.pane, request: rid, text: text, via: .app))
        sending = false
        switch o {
        case .answered:
            next()
        case .refused(let hint):
            // Closed or expired on the hab: show it as answered elsewhere.
            answeredHere.remove(c.id)
            failure = nil
            if live != nil { failure = hint }
        case .failed(let hint), .unreachable(let hint):
            answeredHere.remove(c.id)
            failure = hint
        }
    }
}
