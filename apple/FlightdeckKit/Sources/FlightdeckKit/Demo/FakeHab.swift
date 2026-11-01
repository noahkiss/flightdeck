import Foundation
import HabAPI

/// A hab in memory that speaks the control protocol over a `FakeTransport`:
/// hello and welcome with the revision rule, reconnect tokens per epoch,
/// `events.subscribe` with a snapshot, events with a per-connection seq,
/// heartbeats, and `hab.ping`. Tests drive it; demo mode replays a script on it.
///
/// It follows hab-logic-api.md 3.2 to 3.7. It is not a model of everything the
/// real hab does: unknown methods answer `unavailable`.
public actor FakeHab {
    public nonisolated let hostID: String
    public private(set) var epoch: String

    private var projects: [String: Project] = [:]
    private var sessions: [String: Session] = [:]
    private var panes: [String: Pane] = [:]
    private var agents: [String: AgentState] = [:]
    private var serverRevisions: ClosedRange<Int>
    private var capabilities: [String]
    /// Usage readings by account key, for `usage.list` and `usage.updated`.
    private var usage: [String: UsageAccount] = [:]
    /// How many `usage.list` calls were answered.
    public private(set) var usageLists = 0

    private final class Conn {
        let stream: ByteStream
        var welcomed = false
        var subscribed = false
        var seq: UInt64 = 0
        var clientID = ""
        var heartbeat: Task<Void, Never>?
        init(stream: ByteStream) { self.stream = stream }
    }

    private var conns: [Int: Conn] = [:]
    private var nextConn = 1
    private var nextClient = 1
    /// Reconnect tokens issued in this epoch: token -> client id.
    private var tokens: [String: String] = [:]
    private var silent = false
    private var heartbeatEvery: Duration?

    /// Chat messages by session, oldest first; a session with none has no reader.
    private var chats: [String: [ChatMessage]] = [:]
    /// Change counter per session: the counter value each message last changed at.
    private var chatChanges: [String: [String: Int]] = [:]
    private var chatClock = 0
    /// Whether `session.send` is built. It follows the hab's rules: Claude Code
    /// only, `conflict` while the agent works or waits, `unavailable` with no agent.
    private var sendWorks = true
    private var onSend: (@Sendable (SessionSendParams) async -> Void)?
    /// Demo mode: every harness takes `session.send`, not only Claude Code.
    private var sendAnyHarness = false
    private var nextSendFailure: Failure?
    private var chatPageHook: (@Sendable (ChatPageParams, ChatPagePhase) async -> Void)?

    /// When a `chat.page` hook runs: before the page is read, or after it is
    /// read and before the reply goes out.
    public enum ChatPagePhase: Sendable { case before, after }

    /// Runs `hook` around each `chat.page`, so a test can send events that race the page.
    public func setChatPageHook(_ hook: (@Sendable (ChatPageParams, ChatPagePhase) async -> Void)?) {
        chatPageHook = hook
    }

    /// Every `pane.input` received, oldest first.
    public private(set) var inputs: [InputParams] = []
    /// Every `session.send` received, oldest first.
    public private(set) var sends: [SessionSendParams] = []
    /// How many `chat.page` calls were answered.
    public private(set) var chatPages = 0

    /// Every `push.register` received, oldest first.
    public private(set) var pushRegisters: [PushRegisterParams] = []
    /// Every `push.register_activity` received, oldest first.
    public private(set) var activityRegisters: [PushActivityParams] = []
    /// Every `push.unregister` received, oldest first.
    public private(set) var unregisters: [PushUnregisterParams] = []
    /// Every `session.focus` received, oldest first.
    public private(set) var focusReports: [SessionFocusParams] = []
    /// Every `approval.answer` received, oldest first, refused or not.
    public private(set) var answers: [ApprovalAnswerParams] = []
    /// Every `notice.raise` received, oldest first.
    public private(set) var notices: [NoticeRaiseParams] = []
    private var nextAnswerFailure: Failure?

    /// Makes the next `approval.answer` fail with this error.
    public func failNextAnswer(kind: ErrorKind, hint: String) {
        nextAnswerFailure = Failure(kind: kind, hint: hint)
    }

    /// Every hello received, oldest first.
    public private(set) var hellos: [HelloParams] = []
    /// How many `events.subscribe` calls were answered.
    public private(set) var subscribes = 0
    /// How many `hab.ping` calls were answered.
    public private(set) var pings = 0
    /// Panes with data streams, input and size ownership (`FakePanes`).
    public nonisolated let terminals = FakePanes()

    public init(hostID: String, revisions: ClosedRange<Int> = HabAPI.minRevision...HabAPI.revision, capabilities: [String] = ["agent-state", "data-streams", "snapshot-vt", "transcripts"]) {
        self.hostID = hostID
        self.epoch = Self.randomID()
        self.serverRevisions = revisions
        self.capabilities = capabilities
    }

    /// A transport that reaches this hab. Every open is a new connection.
    public nonisolated func transport(endpoint: String? = nil) -> FakeTransport {
        FakeTransport(endpoint: endpoint ?? "demo") { [self] stream, kind in
            if kind == .data {
                Task { await self.terminals.serve(stream) }
            } else {
                Task { await self.serve(stream) }
            }
        }
    }

    /// Open connections.
    public var connectionCount: Int { conns.count }

    // MARK: - Objects

    /// Loads objects without events, as a state file read at start.
    public func load(projects: [Project], sessions: [Session], panes: [Pane], agents: [AgentState]) {
        for p in projects { self.projects[p.id] = p }
        for s in sessions { self.sessions[s.id] = s }
        for p in panes { self.panes[p.id] = p }
        for a in agents { self.agents[a.pane] = a }
    }

    /// Stores a project with its revision raised by one, and sends `project.updated`.
    public func put(_ p: Project) async {
        var p = p
        p.revision = (projects[p.id]?.revision ?? 0) + 1
        projects[p.id] = p
        await emit(type: "project.updated", id: p.id, revision: p.revision, data: p)
    }

    /// Stores a session with its revision raised by one, and sends `session.updated`.
    public func put(_ s: Session) async {
        var s = s
        s.revision = (sessions[s.id]?.revision ?? 0) + 1
        sessions[s.id] = s
        await emit(type: "session.updated", id: s.id, revision: s.revision, data: s)
    }

    /// Stores a pane with its revision raised by one, and sends `pane.updated`.
    public func put(_ p: Pane) async {
        var p = p
        p.revision = (panes[p.id]?.revision ?? 0) + 1
        panes[p.id] = p
        await emit(type: "pane.updated", id: p.id, revision: p.revision, data: p)
    }

    /// Stores an agent state with its per-pane revision raised by one, and
    /// sends `agent.state`.
    public func put(_ a: AgentState) async {
        var a = a
        a.revision = (agents[a.pane]?.revision ?? 0) + 1
        agents[a.pane] = a
        await emit(type: "agent.state", id: a.pane, revision: a.revision, data: a)
    }

    /// Removes a session and sends its final `session.deleted`.
    public func deleteSession(_ id: String) async {
        guard let s = sessions.removeValue(forKey: id) else { return }
        await emit(type: "session.deleted", id: id, revision: s.revision + 1, data: Deleted())
    }

    public func session(_ id: String) -> Session? { sessions[id] }

    // MARK: - Chat

    /// Loads a session's chat messages without events.
    public func setChat(session: String, _ messages: [ChatMessage]) {
        chats[session] = messages
        chatClock += 1
        chatChanges[session] = Dictionary(messages.map { ($0.id, chatClock) }, uniquingKeysWith: { a, _ in a })
    }

    /// Appends a message, or replaces the one with its id, and sends `chat.message`.
    public func putChat(_ m: ChatMessage) async {
        var list = chats[m.session] ?? []
        if let i = list.firstIndex(where: { $0.id == m.id }) { list[i] = m } else { list.append(m) }
        chats[m.session] = list
        chatClock += 1
        chatChanges[m.session, default: [:]][m.id] = chatClock
        await emit(type: "chat.message", id: m.id, revision: nil, data: m)
    }

    /// Replaces a session's transcript and sends the `reset` event.
    public func resetChat(session: String, _ messages: [ChatMessage]) async {
        setChat(session: session, messages)
        await emit(type: "chat.message", id: "reset-\(chatClock)", revision: nil,
                   data: ChatMessage(at: Fixture.stamp(Date()), event: .reset, id: "reset-\(chatClock)", kind: .event, session: session))
    }

    public func chat(_ session: String) -> [ChatMessage] { chats[session] ?? [] }

    /// Turns `session.send` on, with an optional hook that runs for each send.
    public func setSend(works: Bool, anyHarness: Bool = false, onSend: (@Sendable (SessionSendParams) async -> Void)? = nil) {
        sendWorks = works
        sendAnyHarness = anyHarness
        self.onSend = onSend
    }

    /// Makes the next `session.send` fail with this error, whatever the state.
    public func failNextSend(kind: ErrorKind, hint: String) {
        nextSendFailure = Failure(kind: kind, hint: hint)
    }

    private func chatPage(_ p: ChatPageParams, seq: UInt64) throws -> ChatMessages {
        guard sessions[p.session] != nil || chats[p.session] != nil else {
            throw Failure(kind: .notFound, hint: "no session \(p.session)")
        }
        guard let list = chats[p.session] else {
            throw Failure(kind: .unavailable, hint: "session \(p.session) has no transcript reader")
        }
        let limit = min(max(p.limit ?? 50, 1), 200)
        if let after = p.after {
            guard let since = Int(after), since <= chatClock else { throw Failure(kind: .conflict, hint: "stale cursor") }
            let changes = chatChanges[p.session] ?? [:]
            let changed = list.filter { (changes[$0.id] ?? 0) > since }
            let page = Array(changed.prefix(limit))
            let next = page.count < changed.count ? (page.last.flatMap { changes[$0.id] } ?? since) : chatClock
            return ChatMessages(cursor: String(next), messages: page, more: page.count < changed.count, seq: seq)
        }
        var end = list.count
        if let before = p.before {
            guard let i = list.firstIndex(where: { $0.id == before }) else { throw Failure(kind: .notFound, hint: "no message \(before)") }
            end = i
        }
        let start = max(0, end - limit)
        // An empty transcript has no cursor yet, as when the real file is not written.
        return ChatMessages(cursor: list.isEmpty ? "" : String(chatClock), messages: Array(list[start..<end]), more: start > 0, seq: seq)
    }

    /// The hab's send gate (`harness-adapters.md`, `session.send` built choices).
    private func sendRefusal(_ p: SessionSendParams) -> Failure? {
        if let f = nextSendFailure {
            nextSendFailure = nil
            return f
        }
        guard let s = sessions[p.session] else { return Failure(kind: .notFound, hint: "no session \(p.session)") }
        if s.state == .exited { return Failure(kind: .conflict, hint: "session \(p.session) has exited") }
        guard let pane = s.pane, let a = agents[pane] else {
            return Failure(kind: .unavailable, hint: "session \(p.session) has no agent state")
        }
        guard a.harness == "claude" || sendAnyHarness else { return Failure(kind: .unavailable, hint: "\(a.harness) has no adapter yet") }
        switch a.state {
        case .idle, .done, .error: return nil
        default: return Failure(kind: .conflict, hint: "the agent is \(a.state.rawValue)")
        }
    }

    private struct Failure: Error {
        var kind: ErrorKind
        var hint: String
    }
    public func agent(_ pane: String) -> AgentState? { agents[pane] }

    // MARK: - Faults and lifecycle

    /// A new gff-hab process: new epoch, every connection closed, tokens void.
    /// With `lowerRevisions`, every revision restarts at 1, as after a lost state file.
    public func restart(lowerRevisions: Bool = false) {
        epoch = Self.randomID()
        tokens = [:]
        if lowerRevisions {
            for k in projects.keys { projects[k]?.revision = 1 }
            for k in sessions.keys { sessions[k]?.revision = 1 }
            for k in panes.keys { panes[k]?.revision = 1 }
        }
        // Agent revisions restart with the epoch.
        for k in agents.keys { agents[k]?.revision = 1 }
        dropConnections()
    }

    /// Closes every connection; tokens stay valid, as when the network drops.
    public func dropConnections() {
        for (_, c) in conns {
            c.heartbeat?.cancel()
            c.stream.close()
        }
        conns = [:]
    }

    /// While set, the hab reads but never answers or sends anything.
    public func setSilent(_ on: Bool) {
        silent = on
    }

    /// Sends a heartbeat every `interval` on each welcomed connection; nil stops.
    public func setHeartbeat(_ interval: Duration?) {
        heartbeatEvery = interval
    }

    public func setRevisions(_ r: ClosedRange<Int>) {
        serverRevisions = r
    }

    /// Raises every subscribed connection's seq without sending, so the next
    /// event or heartbeat shows a gap.
    public func skipSeq() {
        for (_, c) in conns where c.subscribed { c.seq += 1 }
    }

    /// Sends a heartbeat with the current seq on every welcomed connection.
    public func sendHeartbeat() async {
        for (_, c) in conns where c.welcomed {
            await notify(c, method: HabNotification.heartbeat, params: Heartbeat(seq: c.seq))
        }
    }

    // MARK: - Usage

    /// Loads usage readings without events and adds the usage capability.
    public func setUsage(_ accounts: [UsageAccount]) {
        if !capabilities.contains("usage") { capabilities.append("usage") }
        for a in accounts { usage[a.key] = a }
    }

    /// Stores one account's reading and sends `usage.updated`.
    public func putUsage(_ a: UsageAccount) async {
        usage[a.key] = a
        await emit(type: "usage.updated", id: a.key, revision: nil, data: a)
    }

    /// Sends a raw event to every subscribed connection, without storing it.
    public func emit<T: Encodable>(type: String, id: String, revision: UInt64?, data: T) async {
        guard let value = try? JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(data)) else { return }
        for (_, c) in conns.sorted(by: { $0.key < $1.key }) where c.subscribed {
            c.seq += 1
            await notify(c, method: HabNotification.event, params: Event(data: value, id: id, revision: revision, seq: c.seq, type: type))
        }
    }

    // MARK: - Serving

    private func serve(_ stream: ByteStream) async {
        let id = nextConn
        nextConn += 1
        let c = Conn(stream: stream)
        conns[id] = c
        var lines = LineBuffer()
        reading: while true {
            guard let chunk = try? await stream.read() else { break }
            guard let batch = try? lines.append(chunk) else { break }
            for line in batch {
                if !(await handle(line, c)) { break reading }
            }
        }
        c.heartbeat?.cancel()
        stream.close()
        if conns[id] === c { conns[id] = nil }
        if c.welcomed, !conns.values.contains(where: { $0.clientID == c.clientID }) {
            await terminals.disconnected(client: c.clientID)
        }
    }

    private struct Head: Decodable {
        var id: Int?
        var method: String?
        var params: JSONValue?
    }

    /// Answers one request. Returns false to close the connection.
    private func handle(_ line: Data, _ c: Conn) async -> Bool {
        guard let req = try? JSONDecoder().decode(Head.self, from: line), let method = req.method else { return true }
        if silent { return true }
        guard let id = req.id else { return true }
        let params = req.params ?? .object([:])
        if !c.welcomed && method != HabMethods.habHello.name {
            await fail(c, id: id, kind: .invalid, hint: "the first request must be hab.hello")
            return false
        }
        switch method {
        case HabMethods.habHello.name:
            if c.welcomed {
                await fail(c, id: id, kind: .invalid, hint: "a second hab.hello")
                return false
            }
            guard let h = try? params.decode(HelloParams.self) else {
                await fail(c, id: id, kind: .invalid, hint: "bad hello")
                return false
            }
            hellos.append(h)
            let chosen = min(h.revision, serverRevisions.upperBound)
            if chosen < h.minRevision || chosen < serverRevisions.lowerBound {
                await fail(c, id: id, kind: .revisionUnsupported, hint: "no API revision fits",
                           client: RevisionRange(max: h.revision, min: h.minRevision),
                           server: RevisionRange(max: serverRevisions.upperBound, min: serverRevisions.lowerBound))
                return false
            }
            if let t = h.reconnectToken, let known = tokens.removeValue(forKey: t) {
                c.clientID = known
                // The older connection with this client id is closed as dead.
                for (k, other) in conns where other !== c && other.clientID == known {
                    other.heartbeat?.cancel()
                    other.stream.close()
                    conns[k] = nil
                }
            } else {
                c.clientID = "c\(nextClient)"
                nextClient += 1
            }
            let token = Self.randomID()
            tokens[token] = c.clientID
            c.welcomed = true
            await reply(c, id: id, result: Welcome(
                build: "fake", capabilities: capabilities, clientId: c.clientID,
                deviceId: "SHA256:fake-" + hostID, epoch: epoch, host: HostInfo(id: hostID),
                reconnectToken: token, revision: chosen, seq: 0))
            startHeartbeat(c)
        case HabMethods.habPing.name:
            pings += 1
            await reply(c, id: id, result: Empty(seq: c.seq))
        case HabMethods.eventsSubscribe.name, HabMethods.structureGet.name:
            if method == HabMethods.eventsSubscribe.name {
                c.subscribed = true
                subscribes += 1
            }
            await reply(c, id: id, result: snapshot(seq: c.seq))
        case HabMethods.dataTicket.name, HabMethods.paneInput.name, HabMethods.paneSetDesiredSize.name,
             HabMethods.paneTake.name, HabMethods.paneRedraw.name:
            await terminalCall(method, params, c, id: id)
        case HabMethods.usageList.name where capabilities.contains("usage"):
            usageLists += 1
            await reply(c, id: id, result: UsageReadings(accounts: usage.values.sorted { $0.key < $1.key }, seq: c.seq))
        case HabMethods.chatPage.name:
            do {
                let p = try params.decode(ChatPageParams.self)
                chatPages += 1
                if let h = chatPageHook { await h(p, .before) }
                let page = try chatPage(p, seq: c.seq)
                if let h = chatPageHook { await h(p, .after) }
                await reply(c, id: id, result: page)
            } catch let f as Failure {
                await fail(c, id: id, kind: f.kind, hint: f.hint)
            } catch {
                await fail(c, id: id, kind: .invalid, hint: "bad chat.page params")
            }
        case HabMethods.sessionSend.name:
            guard sendWorks else {
                await fail(c, id: id, kind: .unavailable, hint: "session.send is not built (fake hab)")
                break
            }
            guard let p = try? params.decode(SessionSendParams.self) else {
                await fail(c, id: id, kind: .invalid, hint: "bad session.send params")
                break
            }
            if let refusal = sendRefusal(p) {
                await fail(c, id: id, kind: refusal.kind, hint: refusal.hint)
                break
            }
            sends.append(p)
            await reply(c, id: id, result: Empty(seq: c.seq))
            if let hook = onSend { Task { await hook(p) } }
        case HabMethods.pushRegister.name, HabMethods.pushRegisterActivity.name:
            guard capabilities.contains("push") else {
                await fail(c, id: id, kind: .unavailable, hint: "push is not set up on this hab; see docs/push.md")
                break
            }
            if method == HabMethods.pushRegister.name, let p = try? params.decode(PushRegisterParams.self) {
                pushRegisters.append(p)
            } else if let p = try? params.decode(PushActivityParams.self) {
                activityRegisters.append(p)
            } else {
                await fail(c, id: id, kind: .invalid, hint: "bad \(method) params")
                break
            }
            await reply(c, id: id, result: Empty(seq: c.seq))
        case HabMethods.pushUnregister.name:
            guard let p = try? params.decode(PushUnregisterParams.self) else {
                await fail(c, id: id, kind: .invalid, hint: "bad push.unregister params")
                break
            }
            unregisters.append(p)
            await reply(c, id: id, result: Empty(seq: c.seq))
        case HabMethods.sessionFocus.name:
            guard let p = try? params.decode(SessionFocusParams.self) else {
                await fail(c, id: id, kind: .invalid, hint: "bad session.focus params")
                break
            }
            focusReports.append(p)
            if p.active && sessions[p.session] == nil {
                await fail(c, id: id, kind: .notFound, hint: "session \(p.session) does not exist")
                break
            }
            await reply(c, id: id, result: Empty(seq: c.seq))
        case HabMethods.noticeRaise.name:
            guard let p = try? params.decode(NoticeRaiseParams.self) else {
                await fail(c, id: id, kind: .invalid, hint: "bad notice.raise params")
                break
            }
            notices.append(p)
            await reply(c, id: id, result: NoticeRaised(id: "n\(notices.count)", pushed: 0, seq: c.seq))
        case HabMethods.approvalAnswer.name:
            guard let p = try? params.decode(ApprovalAnswerParams.self) else {
                await fail(c, id: id, kind: .invalid, hint: "bad approval.answer params")
                break
            }
            answers.append(p)
            if let f = nextAnswerFailure {
                nextAnswerFailure = nil
                await fail(c, id: id, kind: f.kind, hint: f.hint)
                break
            }
            if p.choice == .answer {
                await fail(c, id: id, kind: .unavailable, hint: "answering a question is not built yet; answer it in the terminal")
                break
            }
            // The real hab's rule: only the pane's open request, and only once.
            guard var a = agents[p.pane], a.state == .blocked, a.kind == .permission, let r = a.request?.id, r == p.request else {
                await fail(c, id: id, kind: .conflict, hint: "request \(p.request) is not open on pane \(p.pane)")
                break
            }
            await reply(c, id: id, result: Empty(seq: c.seq))
            a.state = p.choice == .allow ? .working : .idle
            a.kind = nil
            a.request = nil
            a.at = Fixture.stamp(Date())
            await put(a)
        default:
            await fail(c, id: id, kind: .unavailable, hint: "the fake hab does not serve \(method)")
        }
        return true
    }

    /// The pane methods, answered by `terminals`.
    private func terminalCall(_ method: String, _ params: JSONValue, _ c: Conn, id: Int) async {
        do {
            switch method {
            case HabMethods.dataTicket.name:
                let p = try params.decode(TicketParams.self)
                await reply(c, id: id, result: try await terminals.ticket(pane: p.pane, client: c.clientID))
            case HabMethods.paneInput.name:
                let p = try params.decode(InputParams.self)
                inputs.append(p)
                do {
                    try await terminals.input(pane: p.pane, client: c.clientID, data: p.data ?? Data((p.text ?? "").utf8))
                } catch {
                    // A pane the hab lists but the fake terminals do not run still takes input.
                    guard panes[p.pane] != nil else { throw error }
                }
                await reply(c, id: id, result: Empty(seq: c.seq))
            case HabMethods.paneSetDesiredSize.name:
                let p = try params.decode(DesiredSizeParams.self)
                let applied = try await terminals.setDesired(pane: p.pane, client: c.clientID, cols: p.cols, rows: p.rows)
                await reply(c, id: id, result: DesiredSizeResult(applied: applied, seq: c.seq))
            case HabMethods.paneTake.name:
                let p = try params.decode(PaneRef.self)
                let owner = try await terminals.take(pane: p.pane, client: c.clientID)
                await reply(c, id: id, result: OwnerResult(owner: owner, seq: c.seq))
            default:
                let p = try params.decode(RedrawParams.self)
                try await terminals.redraw(pane: p.pane, client: c.clientID, mode: p.mode ?? .snapshot)
                await reply(c, id: id, result: Empty(seq: c.seq))
            }
        } catch let f as FakeFailure {
            await fail(c, id: id, kind: f.kind, hint: f.hint)
        } catch {
            await fail(c, id: id, kind: .invalid, hint: String(describing: error))
        }
    }

    private func snapshot(seq: UInt64) -> StructureResult {
        StructureResult(
            agents: agents.values.sorted { $0.pane < $1.pane },
            host: HostInfo(id: hostID),
            panes: panes.values.sorted { $0.id < $1.id },
            projects: projects.values.sorted { $0.id < $1.id },
            seq: seq,
            sessions: sessions.values.sorted { $0.id < $1.id })
    }

    private func startHeartbeat(_ c: Conn) {
        guard let every = heartbeatEvery else { return }
        c.heartbeat = Task { [weak c] in
            while !Task.isCancelled {
                try? await Task.sleep(for: every)
                guard let c, !Task.isCancelled else { return }
                await self.beat(c)
            }
        }
    }

    private func beat(_ c: Conn) async {
        guard !silent, c.welcomed else { return }
        await notify(c, method: HabNotification.heartbeat, params: Heartbeat(seq: c.seq))
    }

    // MARK: - Wire

    private struct Reply<R: Encodable>: Encodable {
        var jsonrpc = "2.0"
        var id: Int
        var result: R
    }

    private struct ErrorReply: Encodable {
        struct Body: Encodable {
            var code: Int
            var message: String
            var data: ErrorData
        }
        var jsonrpc = "2.0"
        var id: Int
        var error: Body
    }

    private struct Note<P: Encodable>: Encodable {
        var jsonrpc = "2.0"
        var method: String
        var params: P
    }

    private func reply<R: Encodable>(_ c: Conn, id: Int, result: R) async {
        await write(c, Reply(id: id, result: result))
    }

    private func fail(_ c: Conn, id: Int, kind: ErrorKind, hint: String, client: RevisionRange? = nil, server: RevisionRange? = nil) async {
        let data = ErrorData(client: client, hint: hint, kind: kind, seq: c.seq, server: server)
        await write(c, ErrorReply(id: id, error: .init(code: HabAPI.errorCode, message: hint, data: data)))
    }

    private func notify<P: Encodable>(_ c: Conn, method: String, params: P) async {
        guard !silent else { return }
        await write(c, Note(method: method, params: params))
    }

    private func write<T: Encodable>(_ c: Conn, _ value: T) async {
        guard var data = try? JSONEncoder().encode(value) else { return }
        data.append(0x0A)
        if let f = c.stream as? FakeStream {
            // No suspension between a seq being taken and its bytes going out.
            try? f.send(data)
        } else {
            try? await c.stream.write(data)
        }
    }

    static func randomID() -> String {
        String(UInt64.random(in: 0...UInt64.max), radix: 16)
    }
}
