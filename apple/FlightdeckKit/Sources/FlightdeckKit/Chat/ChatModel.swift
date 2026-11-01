import Foundation
import HabAPI
import Observation

/// What the chat view knows about its session, from the hab's objects
/// (app.md 4.1, 4.2). The fleet refreshes it on every hab state change.
public struct ChatContext: Equatable, Sendable {
    public var hostLabel: String = ""
    public var status: HabStatus = .stopped
    public var lostAt: Date?
    public var title: String = ""
    public var harness: String?
    public var model: String?
    public var agent: AgentState?
    public var sessionExited = false
    /// The pane the composer types into: the session's pane, else the target's.
    public var pane: String?
    /// The session is another session's console (app.md 6.3).
    public var isConsole = false

    public init() {}

    public var online: Bool { status == .online }

    /// The agent state the send gate reads; `none` when no agent runs.
    public var agentStatus: AgentStatus {
        if sessionExited { return .exited }
        return agent?.state ?? AgentStatus(rawValue: "none")
    }
}

/// What the composer allows now (app.md 4.2, `harness-adapters.md` Input delivery).
public enum SendGate: Equatable, Sendable {
    /// `idle` or `done`: the message goes now.
    case now
    /// `working`: the message waits for the turn to end; the owner can cancel it.
    case afterTurn
    /// `blocked`: send is off; the pending request shows instead.
    case blocked
    /// `none` or `exited`: send is off; Start or Resume is offered.
    case noAgent
    /// The hab is not online.
    case offline

    /// The hab decides; this only shapes the composer. A pane with no agent
    /// state yet (after a hab restart, until the next hook) still sends, and
    /// the hab's answer shows.
    public static func of(_ c: ChatContext) -> SendGate {
        guard c.online else { return .offline }
        if c.agent == nil && !c.sessionExited { return .now }
        switch c.agentStatus {
        case .idle, .done: return .now
        case .working: return .afterTurn
        case .blocked: return .blocked
        case .error:
            // The turn stopped; the prompt takes input again.
            return .now
        default: return .noAgent
        }
    }

    public var allowsSend: Bool { self == .now || self == .afterTurn }
}

/// A message the owner sent that the transcript does not show yet.
public struct Outgoing: Identifiable, Equatable, Sendable {
    public enum Status: Equatable, Sendable {
        /// Waiting for the turn to end (`working`).
        case queued
        case sending
        /// Written; waiting for the agent to start or the transcript to show it.
        case sent
        /// The agent started working; the transcript shows the record soon.
        case delivered
        /// No working signal and no new user record in time. Retry is manual.
        case notDelivered
        case failed(String)
    }

    public var id: Int
    public var text: String
    public var status: Status
    public var sentAt: Date?
}

/// How the chat view loads (app.md 4.3).
public enum ChatLoad: Equatable, Sendable {
    case loading
    case ready
    /// The transcript file does not exist yet.
    case waiting
    /// The hab cannot serve this session's chat now; the hint says why.
    case unavailable(String)
    case failed(String)
}

/// The chat model of one session: pages from `chat.page` (backwards by id),
/// live `chat.message` events (append, or change in place by id), and the
/// composer's outgoing messages.
///
/// Paging rules (`hab-logic-api.md` 3.9): the tail first; older pages with
/// `before`; after a reconnect, `after: cursor` catches up, and a stale or
/// empty cursor reloads the tail. A `reset` event reloads the tail.
@MainActor
@Observable
public final class ChatModel {
    public let target: SessionTarget
    public private(set) var messages: [ChatMessage] = []
    public private(set) var load: ChatLoad = .loading
    /// Older messages exist on the hab.
    public private(set) var hasOlder = false
    public private(set) var loadingOlder = false
    public private(set) var context = ChatContext()
    public private(set) var outgoing: [Outgoing] = []
    /// The last send or stop error, shown under the composer.
    public private(set) var notice: String?

    /// Page size of `chat.page`.
    @ObservationIgnored public var pageSize = 50
    /// How long a sent message may wait for a working signal or its user record.
    @ObservationIgnored public var readBackWindow: Duration = .seconds(5)
    /// Called after a message reached the hab: the Live Activity tracks the
    /// session the owner last sent a message to (app.md 9.1).
    @ObservationIgnored public var onSent: (@MainActor (SessionTarget) -> Void)?

    /// The session's rich content (app.md 4.7): shown files, images, modified files.
    public let content: ContentModel

    @ObservationIgnored private let connection: HabConnection?
    @ObservationIgnored private var index: [String: Int] = [:]
    @ObservationIgnored private var cursor: String?
    @ObservationIgnored private var listen: Task<Void, Never>?
    @ObservationIgnored private var viewers = 0
    @ObservationIgnored private var loadGen = 0
    /// The seq rule's floors: the oldest connection still current, the last
    /// tail page (every message), and per message the newest page or event.
    @ObservationIgnored private var minGen = 0
    @ObservationIgnored private var floorAll: (gen: Int, seq: UInt64)?
    @ObservationIgnored private var floors: [String: (gen: Int, seq: UInt64)] = [:]
    /// The native session id the tail was loaded for.
    @ObservationIgnored private var tailNative: String?
    /// Events that arrive while the tail loads; applied after it by the seq rule.
    @ObservationIgnored private var buffer: [ChatUpdate]?
    @ObservationIgnored private var nextOutgoing = 1

    public init(target: SessionTarget, connection: HabConnection?) {
        self.target = target
        self.connection = connection
        self.content = ContentModel(target: target, connection: connection)
    }

    /// A model over fixed messages, for previews and snapshots.
    public init(target: SessionTarget, messages: [ChatMessage], context: ChatContext, content: [ContentItem] = []) {
        self.target = target
        self.connection = nil
        self.context = context
        self.content = ContentModel(target: target, items: content, context: context)
        self.load = .ready
        replaceAll(messages)
    }

    // MARK: - Lifecycle

    /// Starts listening and loads the tail. Call when a view appears. Views
    /// share one model, so each `start` needs its own `stop`.
    public func start() {
        viewers += 1
        content.start()
        guard viewers == 1, let conn = connection else { return }
        listen = Task { [weak self] in
            let stream = await conn.chatUpdates()
            for await u in stream {
                guard let self else { return }
                self.receive(u)
            }
        }
        Task { await self.loadTail() }
    }

    /// Stops listening once no view shows the model. The messages stay for a quick return.
    public func stop() {
        guard viewers > 0 else { return }
        viewers -= 1
        content.stop()
        guard viewers == 0 else { return }
        listen?.cancel()
        listen = nil
    }

    private var started: Bool { viewers > 0 }

    /// Takes the hab's new state. A return to online catches up; a new native
    /// session (a cleared conversation, a new agent in the pane, a resume) takes its tail.
    public func update(_ c: ChatContext) {
        let old = context
        guard c != old else { return }
        context = c
        content.update(c)
        if started, c.online, !old.online {
            Task { await self.catchUp() }
        } else if started, c.online, let native = c.agent?.session, native != tailNative {
            Task { await self.loadTail() }
        } else if started, c.online, case .unavailable = load, c.harness != old.harness {
            Task { await self.loadTail() }
        }
        settleOutgoing(old: old)
    }

    // MARK: - Pages

    /// Loads the tail page, replacing what is held.
    public func loadTail() async {
        guard let conn = connection, let session = target.session else {
            load = .unavailable("This pane has no session.")
            return
        }
        guard context.online else { return }
        loadGen += 1
        let gen = loadGen
        buffer = []
        tailNative = context.agent?.session
        if messages.isEmpty { load = .loading }
        do {
            let (page, connGen) = try await conn.callTagged(HabMethods.chatPage, ChatPageParams(limit: pageSize, session: session))
            guard gen == loadGen else { return }
            replaceAll(page.messages)
            for m in page.messages where m.kind == .user { matchOutgoing(m) }
            hasOlder = page.more
            cursor = page.cursor
            minGen = connGen
            floorAll = (connGen, page.seq)
            floors = [:]
            flushBuffer()
            load = messages.isEmpty && (page.cursor ?? "").isEmpty ? .waiting : .ready
        } catch {
            guard gen == loadGen else { return }
            buffer = nil
            load = Self.loadState(for: error)
        }
    }

    /// Loads the page before the oldest message held.
    public func loadOlder() async {
        guard let conn = connection, let session = target.session, hasOlder, !loadingOlder, let first = messages.first else { return }
        let gen = loadGen
        loadingOlder = true
        defer { loadingOlder = false }
        do {
            let page = try await conn.call(HabMethods.chatPage, ChatPageParams(before: first.id, limit: pageSize, session: session))
            // A tail reload or reset since the call: this page belongs to what was replaced.
            guard gen == loadGen else { return }
            let fresh = page.messages.filter { index[$0.id] == nil }
            replaceAll(fresh + messages)
            hasOlder = page.more
        } catch let e as RPCError where e.kind == .notFound {
            guard gen == loadGen else { return }
            // The oldest message is gone (the transcript changed): take the tail again.
            await loadTail()
        } catch {
            guard gen == loadGen else { return }
            notice = "Could not load older messages: \(Self.describe(error))"
        }
    }

    /// The most `after:` pages one catch-up reads before it takes the tail instead.
    static let catchUpPages = 20

    /// After a reconnect: changes since the cursor, else the tail.
    func catchUp() async {
        guard let conn = connection, let session = target.session else { return }
        guard let c = cursor, !c.isEmpty, load == .ready || load == .waiting else {
            await loadTail()
            return
        }
        loadGen += 1
        let gen = loadGen
        buffer = []
        var next = c
        var pageSeqs: [String: UInt64] = [:]
        var lastGen = 0
        var complete = false
        do {
            for _ in 0..<Self.catchUpPages {
                let (page, connGen) = try await conn.callTagged(HabMethods.chatPage, ChatPageParams(after: next, limit: 200, session: session))
                guard gen == loadGen else { return }
                if connGen != lastGen {
                    pageSeqs = [:]
                    lastGen = connGen
                }
                for m in page.messages {
                    // An event newer than this page already holds the message.
                    if let f = floors[m.id], f.gen == connGen, f.seq > page.seq { continue }
                    apply(m)
                    pageSeqs[m.id] = page.seq
                }
                if let n = page.cursor, !n.isEmpty { next = n }
                if !page.more || (page.cursor ?? "").isEmpty {
                    complete = true
                    break
                }
            }
        } catch {
            guard gen == loadGen else { return }
            // A stale cursor (conflict) or anything else: the tail is the record.
            await loadTail()
            return
        }
        guard complete else {
            // Changes still wait past the page budget: the tail is cheaper than a silent gap.
            await loadTail()
            return
        }
        cursor = next
        minGen = max(minGen, lastGen)
        for (id, seq) in pageSeqs { floors[id] = (lastGen, seq) }
        flushBuffer()
        if !messages.isEmpty { load = .ready }
    }

    /// Applies the events that waited for a page, by the seq rule.
    private func flushBuffer() {
        let waiting = buffer ?? []
        buffer = nil
        for u in waiting where !isStale(u) { take(u) }
    }

    /// The seq rule. An event from a connection older than the last page
    /// predates it. On the page's connection, an event at or below the seq of
    /// the page that last carried its message is older than what is held. The
    /// rule holds after the page too: an event and a page reply reach the model
    /// by separate paths, so an older event can arrive after its page.
    private func isStale(_ u: ChatUpdate) -> Bool {
        if u.generation < minGen { return true }
        if let f = floors[u.message.id], u.generation == f.gen, u.seq <= f.seq { return true }
        if let f = floorAll, u.generation == f.gen, u.seq <= f.seq { return true }
        return false
    }

    /// Applies one event and raises its message's floor.
    private func take(_ u: ChatUpdate) {
        floors[u.message.id] = (u.generation, u.seq)
        apply(u.message)
    }

    // MARK: - Events

    private func receive(_ u: ChatUpdate) {
        guard u.message.session == target.session else { return }
        if u.message.kind == .event, u.message.event == .reset {
            // The transcript was replaced: pages in flight are void; take the tail.
            loadGen += 1
            Task { await self.loadTail() }
            return
        }
        if buffer != nil {
            buffer?.append(u)
            return
        }
        if isStale(u) { return }
        take(u)
        content.chatChanged(u.message)
        if load == .waiting { load = .ready }
    }

    /// Replaces a message with the same id in place, else appends it.
    func apply(_ m: ChatMessage) {
        if let i = index[m.id] {
            messages[i] = m
        } else {
            index[m.id] = messages.count
            messages.append(m)
        }
        if m.kind == .user { matchOutgoing(m) }
        rebuildItems()
    }

    private func replaceAll(_ ms: [ChatMessage]) {
        messages = []
        index = [:]
        for m in ms where index[m.id] == nil {
            index[m.id] = messages.count
            messages.append(m)
        }
        rebuildItems()
    }

    /// The chat view's rows. Rebuilt when messages change; an unchanged
    /// message reuses its row, so its Markdown and tool input are not parsed again.
    public private(set) var items: [ChatItem] = []
    @ObservationIgnored private var rowCache = ChatRender.Cache()

    private func rebuildItems() {
        items = ChatRender.items(messages, cache: &rowCache)
    }

    // MARK: - Composer

    public var gate: SendGate { SendGate.of(context) }

    /// Sends the owner's text under the send gate. Empty text does nothing.
    public func send(_ text: String) {
        let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty, gate.allowsSend else { return }
        notice = nil
        let o = Outgoing(id: nextOutgoing, text: t, status: gate == .now ? .sending : .queued)
        nextOutgoing += 1
        outgoing.append(o)
        if o.status == .sending {
            Task { await self.deliver(o.id) }
        }
    }

    /// Cancels a message that waits for the turn.
    public func cancel(_ id: Int) {
        outgoing.removeAll { $0.id == id && $0.status == .queued }
    }

    /// Sends a "not delivered" or failed message again. Never automatic.
    public func retry(_ id: Int) {
        guard let i = outgoing.firstIndex(where: { $0.id == id }), gate.allowsSend else { return }
        outgoing[i].status = gate == .now ? .sending : .queued
        if gate == .now { Task { await self.deliver(id) } }
    }

    /// Drops a message from the composer's list.
    public func dismiss(_ id: Int) {
        outgoing.removeAll { $0.id == id }
    }

    /// Stops the current turn: `session.interrupt`, else Escape through
    /// `pane.input` while the adapter is not built. Never a side effect of send.
    public func interrupt() async {
        guard let conn = connection, let session = target.session else { return }
        do {
            _ = try await conn.call(HabMethods.sessionInterrupt, SessionRef(session: session))
        } catch let e as RPCError where e.kind == .unavailable {
            guard let pane = context.pane else { return }
            do {
                _ = try await conn.call(HabMethods.paneInput, InputParams(pane: pane, text: "\u{1b}"))
            } catch {
                notice = "Stop failed: \(Self.describe(error))"
            }
        } catch {
            notice = "Stop failed: \(Self.describe(error))"
        }
    }

    /// Starts an exited session's harness again.
    public func resume() async {
        guard let conn = connection, let session = target.session else { return }
        do {
            _ = try await conn.call(HabMethods.sessionResume, SessionRef(session: session))
        } catch {
            notice = "Resume failed: \(Self.describe(error))"
        }
    }

    private func deliver(_ id: Int) async {
        guard let conn = connection, let session = target.session,
              let i = outgoing.firstIndex(where: { $0.id == id }) else { return }
        let text = outgoing[i].text
        outgoing[i].status = .sending
        do {
            do {
                _ = try await conn.call(HabMethods.sessionSend, SessionSendParams(origin: .typed, session: session, text: text))
            } catch let e as RPCError where e.kind == .unavailable && typesWithoutAdapter {
                // The harness has no adapter on this hab yet: type it as the adapter would.
                guard let pane = context.pane else { throw e }
                for chunk in Self.typedInput(text) {
                    _ = try await conn.call(HabMethods.paneInput, InputParams(data: chunk, pane: pane))
                }
            }
            guard let j = outgoing.firstIndex(where: { $0.id == id }) else { return }
            outgoing[j].status = .sent
            outgoing[j].sentAt = Date()
            onSent?(target)
            let window = readBackWindow
            Task { [weak self] in
                try? await Task.sleep(for: window)
                self?.readBackExpired(id)
            }
        } catch let e as RPCError where e.kind == .conflict && context.agentStatus == .working {
            // The hab does not queue: keep the text and send it when the turn ends.
            guard let j = outgoing.firstIndex(where: { $0.id == id }) else { return }
            outgoing[j].status = .queued
        } catch {
            guard let j = outgoing.firstIndex(where: { $0.id == id }) else { return }
            outgoing[j].status = .failed(Self.refusal(error))
        }
    }

    /// Whether `pane.input` may stand in for `session.send`: only for a pane
    /// whose harness has no adapter on the hab yet (Claude Code has one), with
    /// a known agent at its prompt. Never for a pane with no agent state.
    private var typesWithoutAdapter: Bool {
        guard let a = context.agent, a.harness != "claude" else { return false }
        return a.state == .idle || a.state == .done || a.state == .error
    }

    /// The hab's reason for refusing, with its error kind.
    static func refusal(_ error: Error) -> String {
        if let e = error as? RPCError, let d = e.data { return "\(d.kind.rawValue): \(d.hint)" }
        return describe(error)
    }

    /// The writes that type a message into a harness prompt: the text in
    /// bracketed paste, then Enter as its own write (`harness-adapters.md`,
    /// Input delivery). A multi-line message stays one message.
    public static func typedInput(_ text: String) -> [Data] {
        // An ESC in the text could end the paste early; nobody types one on purpose.
        let clean = text.replacingOccurrences(of: "\u{1b}", with: "")
        let paste = "\u{1b}[200~" + clean + "\u{1b}[201~"
        return [Data(paste.utf8), Data("\r".utf8)]
    }

    private func readBackExpired(_ id: Int) {
        guard let i = outgoing.firstIndex(where: { $0.id == id }), outgoing[i].status == .sent else { return }
        outgoing[i].status = .notDelivered
    }

    /// A new user record clears the oldest sent message with the same text.
    private func matchOutgoing(_ m: ChatMessage) {
        let text = (m.text ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        if let i = outgoing.firstIndex(where: { [.sending, .sent, .delivered, .notDelivered].contains($0.status) && $0.text == text }) {
            outgoing.remove(at: i)
        }
    }

    /// Applies state changes to the outgoing list: a working signal confirms a
    /// sent message; the end of a turn sends the next queued one.
    private func settleOutgoing(old: ChatContext) {
        let now = context.agentStatus
        if now == .working {
            for i in outgoing.indices where outgoing[i].status == .sent || outgoing[i].status == .notDelivered {
                outgoing[i].status = .delivered
            }
        }
        guard gate == .now, old.agentStatus != now || !old.online else { return }
        // The turn that took them is over; the transcript holds them.
        outgoing.removeAll { $0.status == .delivered }
        if let q = outgoing.firstIndex(where: { $0.status == .queued }) {
            // One at a time: the next waits for the turn this one starts.
            let id = outgoing[q].id
            outgoing[q].status = .sending
            Task { await self.deliver(id) }
        }
    }

    // MARK: - Helpers

    static func loadState(for error: Error) -> ChatLoad {
        if let e = error as? RPCError, e.kind == .unavailable {
            return .unavailable(e.data?.hint ?? e.message)
        }
        if error is ConnectionClosed { return .loading }
        return .failed(describe(error))
    }

    static func describe(_ error: Error) -> String {
        if let e = error as? RPCError { return e.data?.hint ?? e.message }
        return String(describing: error)
    }
}

extension ChatContext {
    /// Builds the context of `target` from one hab's state.
    public static func of(_ target: SessionTarget, hab: Board.Hab) -> ChatContext {
        var c = ChatContext()
        let store = hab.state.store
        c.hostLabel = hab.label
        c.status = hab.state.status
        c.lostAt = hab.state.lostAt
        let session = target.session.flatMap { store.sessions[$0] }
        c.pane = session?.pane ?? target.pane
        c.agent = c.pane.flatMap { store.agents[$0] }
        c.harness = c.agent?.harness
        c.model = session?.model
        c.sessionExited = session?.state == .exited
        c.isConsole = session.map { ConsoleOpener.isConsole($0, in: store) } ?? false
        c.title = session.map { ($0.title?.isEmpty == false ? $0.title! : $0.name) } ?? (c.pane.map { "Pane \($0)" } ?? "Session")
        return c
    }
}
