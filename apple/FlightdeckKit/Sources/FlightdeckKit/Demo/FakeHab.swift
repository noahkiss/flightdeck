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
    /// How far the fake hab's clock runs ahead of this device's, for the
    /// welcome's `time`; nil sends no time, as an older hab.
    private var clockSkew: TimeInterval?
    /// How many `usage.list` calls were answered.
    public private(set) var usageLists = 0
    /// Runs once, after the next `usage.list` answer is read and before it is sent.
    private var usageListHook: (@Sendable () async -> Void)?

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

    /// Rich content (app.md 4.7): items by session, file bytes and diffs by item id.
    private var contentItems: [String: [ContentItem]] = [:]
    private var contentFiles: [String: (data: Data, modified: String)] = [:]
    private var contentDiffs: [String: String] = [:]
    /// The largest run one `content.read` returns; tests make it small to see chunks.
    private var contentChunk = 1 << 20
    private var contentReadHook: (@Sendable (ContentReadParams) async -> Void)?
    public private(set) var contentReads = 0
    /// Content calls running per connection, and the most seen at once.
    private var contentRunning: [ObjectIdentifier: Int] = [:]
    public private(set) var contentPeak = 0
    /// How long each content call takes; tests set it to make calls overlap.
    private var contentDelay: Duration?
    /// Content reads to refuse with `unavailable` before serving again.
    private var contentUnavailable = 0
    /// Runs before each `content.list` reply, so a test can hold one back.
    private var contentListHook: (@Sendable (Int) async -> Void)?
    public private(set) var contentLists = 0

    /// Attachments (app.md 4.4), with the capability `attachments`: uploads by id, and the
    /// stored files. A put names the path the real hab would.
    private var uploads: [String: (put: AttachmentPutParams, data: Data)] = [:]
    public private(set) var storedFiles: [String: (put: AttachmentPutParams, data: Data)] = [:]
    public private(set) var attachmentWrites = 0
    /// The `limit` of each `attachment.get` for file bytes, in order.
    public private(set) var attachmentGetLimits: [Int?] = []
    /// The run size a put names; tests make it small to see several writes.
    private var attachmentChunk = 1 << 20
    /// Writes to refuse with `unavailable` before taking them again.
    private var attachmentWriteFailures = 0
    /// Writes to take before those failures start.
    private var attachmentWritesBeforeFailures = 0
    private var nextAttachment = 1

    public func setAttachmentChunk(_ n: Int) { attachmentChunk = n }
    /// A broken hab: writes answer without taking the bytes.
    private var attachmentStall = false
    public func setAttachmentStall(_ on: Bool) { attachmentStall = on }
    /// Refuses `n` writes, after taking `after` writes first.
    public func failNextAttachmentWrites(_ n: Int, after: Int = 0) {
        attachmentWriteFailures = n
        attachmentWritesBeforeFailures = after
    }
    /// Stores a file as an upload would, for the chat view's thumbnails.
    public func storeAttachment(id: String, name: String, data: Data, type: String = "image/png") {
        storedFiles[id] = (AttachmentPutParams(name: name, session: "", sha256: "", size: UInt64(data.count), type: type), data)
    }

    public func setContentDelay(_ d: Duration?) { contentDelay = d }
    public func failNextContentReads(_ n: Int) { contentUnavailable = n }
    public func setContentListHook(_ hook: (@Sendable (Int) async -> Void)?) { contentListHook = hook }

    private func contentDone(_ c: Conn) {
        contentRunning[ObjectIdentifier(c), default: 1] -= 1
    }

    /// Sets a session's content: its items, the bytes behind them and their diffs.
    public func setContent(session: String, items: [ContentItem], files: [String: Data] = [:], diffs: [String: String] = [:]) {
        contentItems[session] = items
        for (id, d) in files { contentFiles[id] = (d, "2026-01-01T10:00:00.000000000Z") }
        for (id, d) in diffs { contentDiffs[id] = d }
    }

    /// Replaces one file's bytes, with a new modification time.
    public func setContentFile(id: String, data: Data, modified: String) {
        contentFiles[id] = (data, modified)
    }

    public func setContentChunk(_ n: Int) { contentChunk = n }

    /// Runs `hook` before each `content.read`, so a test can change the file between chunks.
    public func setContentReadHook(_ hook: (@Sendable (ContentReadParams) async -> Void)?) {
        contentReadHook = hook
    }

    /// Sends `content.updated` for an item, and keeps it in the session's list.
    public func showContent(_ it: ContentItem) async {
        var list = contentItems[it.session] ?? []
        list.removeAll { $0.id == it.id }
        list.append(it)
        contentItems[it.session] = list
        await emit(type: "content.updated", id: it.id, revision: nil, data: it)
    }

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
    /// Runs before a `push.register` is answered: a test holds it open.
    private var pushRegisterHook: (@Sendable (PushRegisterParams) async -> Void)?
    public func setPushRegisterHook(_ hook: (@Sendable (PushRegisterParams) async -> Void)?) { pushRegisterHook = hook }
    /// Push kinds this hab does not know: a `push.register` naming one is refused as
    /// `invalid`, as a hab built before that kind refuses it.
    private var unknownPushKinds: Set<PushKind> = []
    public func setUnknownPushKinds(_ k: Set<PushKind>) { unknownPushKinds = k }
    /// Every `push.register` refused for an unknown kind, oldest first.
    public private(set) var refusedPushRegisters: [PushRegisterParams] = []
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
    /// Every `trust.answer` received, oldest first, refused or not (app.md 7.10).
    public private(set) var trustAnswers: [TrustAnswerParams] = []
    /// The next `trust.answer` calls that fail, in order, before one is served again.
    private var trustFailures: [Failure] = []
    /// What a served `trust.answer` with `exit` reports as dropped.
    private var trustDropped = 0

    public func setTrustDropped(_ n: Int) { trustDropped = n }

    /// Fails the next `trust.answer` with this kind and hint.
    public func failNextTrust(_ kind: ErrorKind, _ hint: String) {
        trustFailures.append(Failure(kind: kind, hint: hint))
    }

    // MARK: Capcom (capcom.md 7.6, 8.4), with the capability `capcom`

    /// Notices `capcom.open` and `capcom.attach` resolve, by id: the raised ones and `addNotice`.
    private var noticeTitles: [String: String] = [:]
    /// Peers a notification's `host` may name, by label or device id, to the label.
    private var capcomPeers: [String: String] = [:]
    /// Console commands a `command` ref resolves, by `<pane>:<id>`, to their line.
    private var capcomCommands: [String: String] = [:]
    /// The standing Capcom session.
    public private(set) var capcomStanding: String?
    private var capcomCount = 0
    /// What waits for each Capcom session's next message.
    private var capcomWaiting: [String: [CapcomContextItem]] = [:]
    private var nextCapcomFailure: Failure?
    /// Every `capcom.open` received, oldest first.
    public private(set) var capcomOpens: [CapcomOpenParams] = []
    /// Every `capcom.attach` received, oldest first.
    public private(set) var capcomAttaches: [CapcomAttachParams] = []
    /// Every `capcom.detach` received, oldest first.
    public private(set) var capcomDetaches: [CapcomDetachParams] = []
    /// The context each `session.send` to a Capcom session carried, in send order.
    public private(set) var capcomDelivered: [[CapcomContextItem]] = []
    /// How many `capcom.pending` calls were answered.
    public private(set) var capcomPendingReads = 0
    /// How many times `capcom.open` resumed an exited standing conversation.
    public private(set) var capcomResumes = 0

    /// Ends a Capcom session's program, as an exit or a reboot would; the record stays.
    public func exitCapcomSession(_ id: String) async {
        guard var s = sessions[id] else { return }
        s.state = .exited
        s.exit = ExitStatus(code: 0)
        await put(s)
    }
    /// Attaches an item as another client would, with no reply to this app.
    public func attachElsewhere(session: String, notice: String) throws {
        let it = try resolve(CapcomContextRef(kind: .notification, ref: notice), session: session)
        capcomWaiting[session] = try adding([it], to: session)
    }

    /// Adds a notice the Capcom calls resolve.
    public func addNotice(id: String, title: String) { noticeTitles[id] = title }
    /// Lets a notification's `host` name this peer, by label or device id.
    public func addCapcomPeer(label: String, deviceID: String) {
        capcomPeers[label] = label
        capcomPeers[deviceID] = label
    }
    /// Adds a console command a `command` ref resolves.
    public func addCapcomCommand(pane: String, id: UInt64, line: String) { capcomCommands["\(pane):\(id)"] = line }
    /// The Capcom home this hab believes in (capcom.md 7.6): the assignment counter and the
    /// home's device id (nil: none assigned), served in `catalog.get`. Unset, this hab is the
    /// home at assignment 1. A home that is not this hab makes `capcom.open` answer
    /// `unavailable` naming it, and none says to assign one, as the real hab does.
    private var capcomHomeSet: (assignment: UInt64, home: String?)?
    public var capcomHome: (assignment: UInt64, home: String?) { capcomHomeSet ?? (1, deviceID) }
    /// Every `catalog.get` received.
    public private(set) var catalogGets = 0
    /// This hab's device id, as its welcome gives it.
    public nonisolated var deviceID: String { Self.deviceID(host: hostID) }
    /// The device id a fake hab with this host id gives.
    public static func deviceID(host: String) -> String { "SHA256:fake-" + host }
    /// Sets the Capcom home this hab believes in; a nil home for none assigned.
    public func setCapcomHome(assignment: UInt64, home: String?) {
        capcomHomeSet = (assignment, home)
    }
    /// Every `capcom.assign` received, oldest first.
    public private(set) var capcomAssigns: [CapcomAssignParams] = []
    /// The catalog's always-on mark (mesh.md 8).
    public private(set) var alwaysOn = false
    public func setAlwaysOn(_ on: Bool) { alwaysOn = on }
    /// The catalog's generation, raised by `announceCatalog`.
    private var catalogGeneration: UInt64 = 1
    /// While on, `capcom.assign` sends no `catalog.updated` after its answer.
    private var holdAssignAnnounce = false
    public func setHoldAssignAnnounce(_ on: Bool) { holdAssignAnnounce = on }
    /// How long each `catalog.get` waits before it answers; nil answers at once.
    private var catalogDelay: Duration?
    public func setCatalogDelay(_ d: Duration?) { catalogDelay = d }
    /// The next `catalog.get` calls that fail with `unavailable`.
    private var catalogFailures = 0
    public func failNextCatalogGets(_ n: Int) { catalogFailures = n }
    /// Sends `catalog.updated` for this hab's own catalog, as the real hab does when it changes.
    public func announceCatalog() async {
        catalogGeneration += 1
        await emit(type: "catalog.updated", id: deviceID, revision: nil,
                   data: CatalogUpdated(deviceId: deviceID, generation: catalogGeneration))
    }
    /// Sets the Capcom home and announces the changed catalog, as an assignment from the
    /// command line or another device would.
    public func reassignCapcom(assignment: UInt64, home: String?) async {
        setCapcomHome(assignment: assignment, home: home)
        await announceCatalog()
    }
    /// Adds a Capcom session without an event, as a state file read at start; `standing`
    /// makes it the standing conversation.
    public func loadCapcomSession(_ id: String, name: String, pane: String, standing: Bool) {
        sessions[id] = Session(dir: "/state/capcom/\(id)", id: id, kind: .capcom, name: name, pane: pane, revision: 1, state: .running)
        panes[pane] = Fixture.pane(pane, session: id)
        agents[pane] = Fixture.agent(pane: pane, harness: "claude", state: .idle)
        capcomCount += 1
        if standing { capcomStanding = id }
    }
    /// Makes the next Capcom call fail with this error.
    public func failNextCapcom(kind: ErrorKind, hint: String) { nextCapcomFailure = Failure(kind: kind, hint: hint) }
    /// What waits for a Capcom session's next message.
    public func capcomPending(_ session: String) -> [CapcomContextItem] { capcomWaiting[session] ?? [] }

    // MARK: The Capcom service (capcom-service.md 7), with the capability `capcom-thread`

    /// While on, `capcom.open` answers the service session (kind capcom, no pane), and the
    /// thread calls and events run (capcom-service.md 7.4).
    public private(set) var capcomService = false
    /// The thread, oldest first, folded by id.
    public private(set) var capcomThread: [CapcomItem] = []
    private var capcomThreadN = 0
    /// How many `capcom.interrupt` calls were answered.
    public private(set) var capcomInterrupts = 0
    /// Every `capcom.thread.page` received, oldest first.
    public private(set) var capcomPages: [CapcomThreadPageParams] = []
    private var capcomInterruptUnavailable = false
    /// Pending proposals `logbook.list` answers for a session (capcom-service.md 6.4).
    private var capcomProposals: [String: [LogbookRecord]] = [:]

    public func setCapcomService(_ on: Bool) {
        capcomService = on
        setCapability("capcom-thread", on)
        setCapability("capcom", on || capabilities.contains("capcom"))
    }

    /// A new thread item id, after every one so far.
    public func nextCapcomItemID() -> String {
        capcomThreadN += 1
        return String(format: "c%016d", capcomThreadN)
    }

    /// Adds or replaces a thread item and sends `capcom.item`.
    @discardableResult
    public func putCapcomItem(_ it: CapcomItem) async -> CapcomItem {
        if let i = capcomThread.firstIndex(where: { $0.id == it.id }) {
            capcomThread[i] = it
        } else {
            capcomThread.append(it)
            capcomThread.sort { $0.id < $1.id }
        }
        await emit(type: "capcom.item", id: it.id, revision: nil, data: it)
        return it
    }

    /// Adds an item of `kind` by `author`, with a fresh id.
    @discardableResult
    public func addCapcomItem(kind: String, author: String, text: String? = nil, tools: [CapcomToolCall]? = nil, card: CapcomCard? = nil,
                              summary: String? = nil, event: Bool? = nil, address: String? = nil) async -> CapcomItem {
        let it = CapcomItem(author: CapcomAuthor(address: address, kind: CapcomAuthorKind(rawValue: author)), card: card, event: event, id: nextCapcomItemID(),
                            kind: CapcomItemKind(rawValue: kind), summary: summary, text: text, time: Fixture.stamp(Date()), tools: tools)
        return await putCapcomItem(it)
    }

    /// Sends `capcom.typing` for the service session.
    public func setCapcomTyping(_ on: Bool) async {
        guard let sid = capcomStanding else { return }
        await emit(type: "capcom.typing", id: sid, revision: nil, data: CapcomTyping(on: on))
    }

    /// Marks a user message read, as the engine's `input.taken` does.
    public func markCapcomRead(_ id: String) async {
        guard var it = capcomThread.first(where: { $0.id == id }) else { return }
        it.read = Fixture.stamp(Date())
        await putCapcomItem(it)
    }

    public func setCapcomInterruptUnavailable(_ on: Bool) { capcomInterruptUnavailable = on }

    /// The sites "Allow this site" kept (capcom-service.md 5.3).
    public private(set) var capcomSites: [String] = []
    public func setCapcomSites(_ s: [String]) { capcomSites = s }

    public func setCapcomProposals(session: String, _ records: [LogbookRecord]) { capcomProposals[session] = records }
    private var nextAnswerFailure: Failure?

    /// Makes the next `approval.answer` fail with this error.
    public func failNextAnswer(kind: ErrorKind, hint: String) {
        nextAnswerFailure = Failure(kind: kind, hint: hint)
    }

    // MARK: Speech (voice.md, "API addition")

    /// What `speech.prepare` answers while the hab has the `speech` capability.
    private var speechPrepare = SpeechStatus(formats: [.opus, .pcm16], model: "parakeet-tdt-0.6b-v2", seq: 0, state: .ready)
    /// What `speech.transcribe` answers: the text for each clip, in order; the
    /// last one repeats. Empty: the clip's byte count as words, for demo mode.
    private var speechTexts: [String] = []
    private var nextSpeechFailure: Failure?
    /// Every `speech.prepare` received.
    public private(set) var speechPrepares = 0
    /// Every `speech.transcribe` received, oldest first.
    public private(set) var transcribes: [SpeechTranscribeParams] = []

    /// Sets what `speech.prepare` answers.
    public func setSpeechPrepare(_ r: SpeechStatus) {
        speechPrepare = r
    }

    /// Sets the texts `speech.transcribe` returns, one per clip; the last repeats.
    public func setTranscripts(_ texts: [String]) {
        speechTexts = texts
    }

    /// Makes the next `speech.transcribe` fail with this error.
    public func failNextTranscribe(kind: ErrorKind, hint: String) {
        nextSpeechFailure = Failure(kind: kind, hint: hint)
    }

    // MARK: - Confirmations

    /// Open and running confirmations (capcom.md 6.1), in arrival order.
    private var confirms: [Confirmation] = []
    /// Closed confirmations, newest first, for `confirm.list {all}`.
    private var closedConfirms: [Confirmation] = []
    /// Runs before `confirm.answer` closes a confirmed start: it may return a
    /// state (failed, unknown) and reason in place of done.
    private var confirmOutcome: (state: ConfirmState, reason: String)?
    public func setConfirmOutcome(_ state: ConfirmState, reason: String) { confirmOutcome = (state, reason) }
    /// Runs after a subscribe's snapshot is taken and before it is sent, so
    /// events it raises reach the client ahead of the snapshot.
    private var subscribeHook: (@Sendable () async -> Void)?
    public func setSubscribeHook(_ hook: (@Sendable () async -> Void)?) { subscribeHook = hook }
    /// What `confirm.list` names as targets.
    private var confirmTargets: [ConfirmTarget] = []
    /// Every `confirm.answer` the hab took, answered or refused.
    public private(set) var confirmAnswers: [ConfirmAnswerParams] = []
    public private(set) var confirmLists = 0

    public func setConfirmTargets(_ t: [ConfirmTarget]) { confirmTargets = t }

    /// Stores a confirmation and sends `confirm.requested`.
    public func requestConfirmation(_ c: Confirmation) async {
        confirms.removeAll { $0.id == c.id }
        confirms.append(c)
        await emit(type: "confirm.requested", id: c.id, revision: nil, data: c)
    }

    /// Closes a confirmation and sends `confirm.closed`.
    public func closeConfirmation(_ id: String, state: ConfirmState, reason: String? = nil, result: ConfirmResult? = nil, answeredBy: String? = nil) async {
        guard let i = confirms.firstIndex(where: { $0.id == id }) else { return }
        var c = confirms.remove(at: i)
        c.state = state
        c.reason = reason
        c.result = result
        c.answeredBy = answeredBy ?? c.answeredBy
        c.closed = Fixture.stamp(Date())
        closedConfirms.insert(c, at: 0)
        await emit(type: "confirm.closed", id: c.id, revision: nil, data: c)
    }

    /// The real hab's rule (capcom.md 6.4): only an open question takes an
    /// answer, and only the first; the edits run.
    private func confirmAnswer(_ p: ConfirmAnswerParams, client: String) async throws -> Confirmation {
        confirmAnswers.append(p)
        guard let c = confirms.first(where: { $0.id == p.id }) else {
            throw FakeFailure(kind: .conflict, hint: "confirmation \(p.id) was already answered")
        }
        guard c.state == .open, c.record != true else {
            throw FakeFailure(kind: .conflict, hint: "confirmation \(p.id) is \(c.state.rawValue)")
        }
        if p.choice == .cancel {
            await closeConfirmation(p.id, state: .cancelled, reason: "cancelled by the user", answeredBy: client)
            return c
        }
        var start = c.start
        var edited = false
        if let e = p.edits, var st = start {
            func apply(_ dst: inout String, _ v: String?) {
                if let v, !v.isEmpty, v != dst { dst = v; edited = true }
            }
            func apply(_ dst: inout String?, _ v: String?) {
                if let v, !v.isEmpty, v != dst { dst = v; edited = true }
            }
            if let h = e.host, !h.isEmpty, h != st.host, (e.folder ?? "").isEmpty {
                throw FakeFailure(kind: .invalid, hint: "a new host needs its folder: give folder with host")
            }
            apply(&st.host, e.host)
            apply(&st.folder, e.folder)
            apply(&st.harness, e.harness)
            apply(&st.model, e.model)
            apply(&st.seed, e.seed)
            start = st
        }
        var group = c.group
        if let plan = p.edits?.plan, var g = group, plan != g.plan {
            guard !plan.items.isEmpty else { throw FakeFailure(kind: .invalid, hint: "the plan needs at least one item") }
            g.plan = plan
            group = g
            edited = true
        }
        if let i = confirms.firstIndex(where: { $0.id == p.id }) {
            confirms[i].start = start
            confirms[i].group = group
            confirms[i].edited = edited
        }
        var out = c
        out.group = group
        out.start = start
        out.edited = edited
        out.answeredBy = client
        if let o = confirmOutcome {
            confirmOutcome = nil
            await closeConfirmation(p.id, state: o.state, reason: o.reason, answeredBy: client)
            out.state = o.state
            out.reason = o.reason
            return out
        }
        let res = ConfirmResult(host: start?.host ?? hostID, name: start?.name, pane: "%9", session: "s-" + p.id.prefix(6))
        await closeConfirmation(p.id, state: .done, result: res, answeredBy: client)
        out.state = .done
        out.result = res
        return out
    }

    // MARK: - Groups

    private var groupBook: [String: Group] = [:]
    /// Every `group.*` method called with its group id (and member, for a retry), oldest first.
    public private(set) var groupCalls: [String] = []
    public func group(_ id: String) -> Group? { groupBook[id] }

    /// Stores a group record at its next revision and sends `group.updated`.
    public func putGroup(_ g: Group) async {
        var g = g
        g.revision = max(g.revision, (groupBook[g.id]?.revision ?? 0) + 1)
        groupBook[g.id] = g
        await emit(type: "group.updated", id: g.id, revision: g.revision, data: g)
    }

    /// Stores a group as a record read at start would, with no event.
    public func loadGroup(_ g: Group) { groupBook[g.id] = g }

    /// The hab's rules for the user's calls (groups.md 5.6, 15.1), on the record alone.
    private func groupCall(_ method: String, _ params: JSONValue, _ c: Conn, id: Int) async {
        do {
            if method == HabMethods.groupRetry.name {
                let p = try decode(GroupRetryParams.self, params, method)
                groupCalls.append("\(method) \(p.group) \(p.member)")
                guard var g = groupBook[p.group] else { throw FakeFailure(kind: .notFound, hint: "no group \(p.group)") }
                guard let i = g.members.firstIndex(where: { $0.name == p.member }) else {
                    throw FakeFailure(kind: .notFound, hint: "group \(g.id) has no member \(p.member)")
                }
                guard g.members[i].state == "failed" || g.members[i].state == "exited" else {
                    throw FakeFailure(kind: .conflict, hint: "member \(p.member) is \(g.members[i].state)")
                }
                g.members[i].state = "planned"
                g.members[i].reason = nil
                await putGroup(g)
                await reply(c, id: id, result: GroupResult(group: groupBook[g.id]!, seq: c.seq))
                return
            }
            let p = try decode(GroupRef.self, params, method)
            groupCalls.append("\(method) \(p.group)")
            guard var g = groupBook[p.group] else { throw FakeFailure(kind: .notFound, hint: "no group \(p.group)") }
            let live = g.state == "running" || g.state == "paused"
            switch method {
            case HabMethods.groupGet.name:
                break
            case HabMethods.groupPause.name:
                guard live else { throw FakeFailure(kind: .conflict, hint: "group \(g.id) is \(g.state)") }
                g.pause.user = Fixture.stamp(Date())
                g.state = "paused"
            case HabMethods.groupResume.name:
                guard g.state == "paused" else { throw FakeFailure(kind: .conflict, hint: "group \(g.id) is \(g.state), not paused") }
                g.pause.user = nil
                if g.pause.usage == nil { g.state = "running" }
            case HabMethods.groupClose.name:
                guard live else { throw FakeFailure(kind: .conflict, hint: "group \(g.id) is \(g.state)") }
                g.state = "ended"
                g.ended = Fixture.stamp(Date())
            default:
                throw FakeFailure(kind: .unavailable, hint: "the fake hab does not serve \(method)")
            }
            if method != HabMethods.groupGet.name { await putGroup(g) }
            await reply(c, id: id, result: GroupResult(group: groupBook[g.id]!, seq: c.seq))
        } catch let f as FakeFailure {
            await fail(c, id: id, kind: f.kind, hint: f.hint)
        } catch let f as Failure {
            await fail(c, id: id, kind: f.kind, hint: f.hint)
        } catch {
            await fail(c, id: id, kind: .invalid, hint: String(describing: error))
        }
    }

    // MARK: - Rounds

    private var roundBook = FakeRounds()
    /// Every `round.*` method called, oldest first.
    public private(set) var roundCalls: [String] = []
    /// Whether a run by hand starts its session at once (true) or stays queued.
    private var roundSpawn = true
    public func setRoundSpawn(_ on: Bool) { roundSpawn = on }
    public var rounds: [Round] { roundBook.list() }
    public func round(named n: String) -> Round? { roundBook.list().first { $0.name == n } }
    public func roundRun(_ id: String) -> RoundRun? { roundBook.run(id) }

    /// Moves a run to a new state and sends `round.run_updated`. A final state stamps the
    /// end and then, as the hab does, sends `round.updated` with the round at its next
    /// revision; a succeeded run of a round with `on_finish` close exits its session.
    public func setRunState(_ id: String, _ state: RoundRunState, reason: String? = nil, result: String? = nil) async {
        guard var run = roundBook.run(id) else { return }
        run.state = state
        run.reason = reason
        run.result = result
        if state.isFinal { run.endedAt = Fixture.stamp(Date()) }
        roundBook.put(run)
        await emit(type: "round.run_updated", id: run.id, revision: nil, data: run)
        if state.isFinal, let r = roundBook.bump(run.round) { await roundUpdated(r) }
        if state == .succeeded, roundBook.rounds[run.round]?.onFinish ?? .close == .close, let sid = run.session, var sess = sessions[sid] {
            sess.state = .exited
            sess.revision += 1
            await put(sess)
        }
    }

    /// Stores a round as a state file read at start would, with no event.
    public func loadRound(_ spec: RoundSpec) throws {
        _ = try roundBook.create(RoundCreateParams(round: spec), projects: projects, sessions: sessions)
    }

    private func roundUpdated(_ r: Round) async {
        await emit(type: "round.updated", id: r.id, revision: r.revision, data: r)
    }

    /// Spawns a queued run's session, named after the round with `Session.round`, as the
    /// real hab's command runs do, then sends the run's `running`.
    private func spawn(_ queued: RoundRun) async -> RoundRun {
        await emit(type: "round.run_updated", id: queued.id, revision: nil, data: queued)
        guard roundSpawn, let r = roundBook.rounds[queued.round] else { return queued }
        let pane = "%r\(queued.round)-\(queued.number)"
        let sid = "s-\(queued.id)"
        let taken = Set(sessions.values.map(\.name))
        var name = r.name
        var n = 2
        while taken.contains(name) { name = "\(r.name)-\(n)"; n += 1 }
        await put(Pane(cols: 120, holderBuild: "fake", id: pane, offset: 0, revision: 1, rows: 40, session: sid))
        await put(Session(id: sid, name: name, pane: pane, project: r.project, revision: 1,
                          round: SessionRound(round: r.id, run: queued.id), state: .running))
        var run = queued
        run.state = .running
        run.session = sid
        run.startedAt = Fixture.stamp(Date())
        roundBook.put(run)
        await emit(type: "round.run_updated", id: run.id, revision: nil, data: run)
        return run
    }

    private func roundCall(_ method: String, _ params: JSONValue, _ c: Conn, id: Int) async {
        roundCalls.append(method)
        do {
            switch method {
            case HabMethods.roundList.name:
                await reply(c, id: id, result: RoundList(rounds: roundBook.list(), seq: c.seq))
            case HabMethods.roundGet.name:
                let r = try roundBook.entry(try decode(RoundRef.self, params, method))
                await reply(c, id: id, result: RoundResult(round: r, seq: c.seq))
            case HabMethods.roundCreate.name:
                let p = try decode(RoundCreateParams.self, params, method)
                let r = try roundBook.create(p, projects: projects, sessions: sessions)
                await roundUpdated(r)
                var run: RoundRun?
                if p.runNow == true { run = await spawn(try roundBook.startRun(RoundRef(id: r.id))) }
                await reply(c, id: id, result: RoundCreated(round: roundBook.rounds[r.id] ?? r, run: run, seq: c.seq))
            case HabMethods.roundUpdate.name:
                let r = try roundBook.update(try decode(RoundUpdateParams.self, params, method), projects: projects, sessions: sessions)
                await roundUpdated(r)
                await reply(c, id: id, result: RoundResult(round: r, seq: c.seq))
            case HabMethods.roundDelete.name:
                let r = try roundBook.delete(try decode(RoundRef.self, params, method))
                await roundUpdated(r)
                await reply(c, id: id, result: Empty(seq: c.seq))
            case HabMethods.roundPause.name, HabMethods.roundUnpause.name:
                let ref = try decode(RoundRef.self, params, method)
                if let r = try roundBook.setPaused(ref, method == HabMethods.roundPause.name) { await roundUpdated(r) }
                await reply(c, id: id, result: RoundResult(round: try roundBook.entry(ref), seq: c.seq))
            case HabMethods.roundRun.name:
                let run = await spawn(try roundBook.startRun(try decode(RoundRef.self, params, method)))
                await reply(c, id: id, result: RoundRunResult(run: run, seq: c.seq))
            case HabMethods.roundRuns.name:
                let page = try roundBook.page(try decode(RoundRunsParams.self, params, method))
                await reply(c, id: id, result: RoundRuns(more: page.more ? true : nil, runs: page.runs, seq: c.seq))
            default:
                await fail(c, id: id, kind: .unavailable, hint: "the fake hab does not serve \(method)")
            }
        } catch let f as FakeFailure {
            await fail(c, id: id, kind: f.kind, hint: f.hint)
        } catch let f as Failure {
            await fail(c, id: id, kind: f.kind, hint: f.hint)
        } catch {
            await fail(c, id: id, kind: .invalid, hint: String(describing: error))
        }
    }

    /// Adds or drops a capability; the next hello carries the new list.
    public func setCapability(_ name: String, _ on: Bool) {
        capabilities.removeAll { $0 == name }
        if on { capabilities.append(name) }
    }

    /// Every hello received, oldest first.
    public private(set) var hellos: [HelloParams] = []
    /// How many `events.subscribe` calls were answered.
    public private(set) var subscribes = 0
    /// How many `hab.ping` calls were answered.
    public private(set) var pings = 0
    /// Panes with data streams, input and size ownership (`FakePanes`).
    public nonisolated let terminals = FakePanes()

    public init(hostID: String, revisions: ClosedRange<Int> = HabAPI.minRevision...HabAPI.revision, capabilities: [String] = ["agent-state", "data-streams", "snapshot-vt", "transcripts", "content"]) {
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
        // The service thread takes every message, never refused as busy (capcom-service.md 7.4).
        if capcomService, s.kind == .capcom, (s.pane ?? "").isEmpty { return nil }
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

    /// Sets the welcome's `time` to this device's clock plus `skew`; nil sends none.
    public func setClockSkew(_ skew: TimeInterval?) {
        clockSkew = skew
    }

    /// Runs `hook` once between reading the next `usage.list` answer and sending it.
    public func setUsageListHook(_ hook: (@Sendable () async -> Void)?) {
        usageListHook = hook
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
                reconnectToken: token, revision: chosen, seq: 0,
                time: clockSkew.map { Fixture.stamp(Date().addingTimeInterval($0)) }))
            startHeartbeat(c)
        case HabMethods.habPing.name:
            pings += 1
            await reply(c, id: id, result: Empty(seq: c.seq))
        case HabMethods.eventsSubscribe.name, HabMethods.structureGet.name:
            if method == HabMethods.eventsSubscribe.name {
                c.subscribed = true
                subscribes += 1
            }
            let snap = snapshot(seq: c.seq)
            if method == HabMethods.eventsSubscribe.name, let hook = subscribeHook {
                subscribeHook = nil
                await hook()
            }
            await reply(c, id: id, result: snap)
        case HabMethods.dataTicket.name, HabMethods.paneInput.name, HabMethods.paneSetDesiredSize.name,
             HabMethods.paneTake.name, HabMethods.paneRedraw.name:
            await terminalCall(method, params, c, id: id)
        case HabMethods.usageList.name where capabilities.contains("usage"):
            usageLists += 1
            // Read the answer first: events the hook sends go out before the reply, as on a busy hab.
            let res = UsageReadings(accounts: usage.values.sorted { $0.key < $1.key }, seq: c.seq)
            if let h = usageListHook {
                usageListHook = nil
                await h()
            }
            await reply(c, id: id, result: res)
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
        case HabMethods.contentList.name, HabMethods.contentRead.name, HabMethods.contentDiff.name:
            // Off the read loop, with the real hab's limit of 4 content calls per connection.
            guard contentRunning[ObjectIdentifier(c), default: 0] < 4 else {
                await fail(c, id: id, kind: .unavailable, hint: "4 content calls already run on this connection")
                break
            }
            contentRunning[ObjectIdentifier(c), default: 0] += 1
            contentPeak = max(contentPeak, contentRunning[ObjectIdentifier(c)] ?? 0)
            Task {
                if let d = self.contentDelay { try? await Task.sleep(for: d) }
                await self.contentCall(method, params, c, id: id)
                self.contentDone(c)
            }
        case HabMethods.attachmentPut.name, HabMethods.attachmentWrite.name, HabMethods.attachmentGet.name:
            await attachmentCall(method, params, c, id: id)
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
            if let ids = p.attachments, let missing = ids.first(where: { storedFiles[$0] == nil }) {
                await fail(c, id: id, kind: .notFound, hint: "no attachment \(missing) for session \(p.session)")
                break
            }
            // The real hab's rule (capcom.md 8.4): an empty send only carries waiting context.
            let carried = capcomWaiting[p.session] ?? []
            if p.text.isEmpty, (p.attachments ?? []).isEmpty, carried.isEmpty {
                await fail(c, id: id, kind: .invalid, hint: "empty message")
                break
            }
            sends.append(p)
            if sessions[p.session]?.kind == .capcom {
                capcomDelivered.append(carried)
                capcomWaiting[p.session] = nil
            }
            await reply(c, id: id, result: Empty(seq: c.seq))
            if capcomService, sessions[p.session]?.kind == .capcom, (sessions[p.session]?.pane ?? "").isEmpty {
                let it = CapcomItem(attachments: p.attachments, author: CapcomAuthor(device: "fake-device", kind: CapcomAuthorKind(rawValue: "user")),
                                    context: carried.isEmpty ? nil : carried, id: nextCapcomItemID(), kind: CapcomItemKind(rawValue: "message"),
                                    text: p.text, time: Fixture.stamp(Date()))
                await putCapcomItem(it)
            }
            if let hook = onSend { Task { await hook(p) } }
        case HabMethods.pushRegister.name, HabMethods.pushRegisterActivity.name:
            guard capabilities.contains("push") else {
                await fail(c, id: id, kind: .unavailable, hint: "push is not set up on this hab; see docs/push.md")
                break
            }
            if method == HabMethods.pushRegister.name, let p = try? params.decode(PushRegisterParams.self) {
                if let k = p.kinds.first(where: { unknownPushKinds.contains($0) }) {
                    refusedPushRegisters.append(p)
                    let known = ["permission", "question", "auth", "error", "done"]
                    await fail(c, id: id, kind: .invalid, hint: "kind \"\(k.rawValue)\" is not one of [\(known.joined(separator: " "))]")
                    break
                }
                pushRegisters.append(p)
                if let hook = pushRegisterHook { await hook(p) }
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
            noticeTitles["n\(notices.count)"] = p.title
            await reply(c, id: id, result: NoticeRaised(id: "n\(notices.count)", pushed: 0, seq: c.seq))
        case HabMethods.capcomThreadPage.name:
            guard capcomService else {
                await fail(c, id: id, kind: .unavailable, hint: "capcom.thread.page needs the Capcom service (fake hab)")
                break
            }
            let p = (try? params.decode(CapcomThreadPageParams.self)) ?? CapcomThreadPageParams()
            capcomPages.append(p)
            let limit = min(max(p.limit ?? 200, 1), 200)
            var older = capcomThread
            if let b = p.before { older = older.filter { $0.id < b } }
            let page = Array(older.reversed().prefix(limit))
            await reply(c, id: id, result: CapcomThreadPage(items: page, more: older.count > page.count, seq: c.seq))
        case HabMethods.capcomStatus.name where capcomService:
            await reply(c, id: id, result: CapcomServiceStatus(connected: true, folder: "/state/capcom/engine", sdkSessions: [], seq: c.seq,
                                                               session: capcomStanding, sites: capcomSites, state: "running"))
        case HabMethods.capcomSiteRemove.name where capcomService:
            guard let p = try? params.decode(CapcomSiteParams.self), capcomSites.contains(p.site) else {
                await fail(c, id: id, kind: .notFound, hint: "Capcom has no allowed site")
                break
            }
            capcomSites.removeAll { $0 == p.site }
            await reply(c, id: id, result: CapcomServiceStatus(connected: true, folder: "/state/capcom/engine", sdkSessions: [], seq: c.seq,
                                                               session: capcomStanding, sites: capcomSites, state: "running"))
        case HabMethods.capcomInterrupt.name:
            guard capcomService, !capcomInterruptUnavailable else {
                await fail(c, id: id, kind: .unavailable, hint: "capcom.interrupt: no engine is connected")
                break
            }
            capcomInterrupts += 1
            await reply(c, id: id, result: Empty(seq: c.seq))
        case HabMethods.logbookList.name where capabilities.contains("logbook"):
            let p = (try? params.decode(LogbookListParams.self)) ?? LogbookListParams()
            await reply(c, id: id, result: LogbookRecords(records: capcomProposals[p.session ?? ""] ?? [], seq: c.seq))
        case HabMethods.capcomOpen.name, HabMethods.capcomAttach.name, HabMethods.capcomDetach.name, HabMethods.capcomPending.name,
             HabMethods.capcomAssign.name:
            await capcomCall(method, params, c, id: id)
        case HabMethods.catalogGet.name where capabilities.contains("catalog") || capabilities.contains("capcom"):
            catalogGets += 1
            if catalogFailures > 0 {
                catalogFailures -= 1
                await fail(c, id: id, kind: .unavailable, hint: "the catalog could not be read")
                break
            }
            if let d = catalogDelay {
                // Answer later, off this connection's read loop, with the catalog as it is then.
                Task {
                    try? await Task.sleep(for: d)
                    await self.replyCatalog(c, id: id)
                }
                break
            }
            await replyCatalog(c, id: id)
        case HabMethods.confirmAnswer.name where capabilities.contains(ConfirmBook.capability):
            guard let p = try? params.decode(ConfirmAnswerParams.self) else {
                await fail(c, id: id, kind: .invalid, hint: "bad confirm.answer params")
                break
            }
            do {
                let out = try await confirmAnswer(p, client: c.clientID)
                await reply(c, id: id, result: ConfirmationResult(confirmation: out, seq: c.seq))
            } catch let f as FakeFailure {
                await fail(c, id: id, kind: f.kind, hint: f.hint)
            } catch {
                await fail(c, id: id, kind: .invalid, hint: String(describing: error))
            }
        case HabMethods.confirmList.name where capabilities.contains(ConfirmBook.capability):
            confirmLists += 1
            let all = (try? params.decode(ConfirmListParams.self))?.all ?? false
            await reply(c, id: id, result: ConfirmList(confirmations: all ? confirms + closedConfirms : confirms, seq: c.seq, targets: confirmTargets))
        case let m where m.hasPrefix("round.") && capabilities.contains(RoundCenter.capability):
            await roundCall(m, params, c, id: id)
        case let m where m.hasPrefix("group.") && capabilities.contains(GroupCenter.capability):
            await groupCall(m, params, c, id: id)
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
            // A Capcom approval card (capcom-service.md 5.3): the first answer wins, and only a
            // choice the card offers.
            if capcomService, p.pane == CapcomThread.engineRoute || p.pane.isEmpty {
                guard var it = capcomThread.first(where: { $0.card?.ref == p.request }), it.answer == nil else {
                    await fail(c, id: id, kind: .conflict, hint: "Capcom approval \(p.request) is not open: answered already, or expired")
                    break
                }
                guard it.card?.choices.contains(p.choice.rawValue) == true else {
                    await fail(c, id: id, kind: .invalid, hint: "Capcom approval \(p.request) does not offer \(p.choice.rawValue)")
                    break
                }
                await reply(c, id: id, result: Empty(seq: c.seq))
                it.answer = CapcomAnswer(by: "fake-device", choice: p.choice.rawValue, time: Fixture.stamp(Date()))
                await putCapcomItem(it)
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
        case HabMethods.trustAnswer.name where capabilities.contains(TrustCard.capability):
            guard let p = try? params.decode(TrustAnswerParams.self) else {
                await fail(c, id: id, kind: .invalid, hint: "bad trust.answer params")
                break
            }
            trustAnswers.append(p)
            if !trustFailures.isEmpty {
                let f = trustFailures.removeFirst()
                await fail(c, id: id, kind: f.kind, hint: f.hint)
                break
            }
            // The real hab's rule: only a pane that shows the prompt.
            guard var a = agents[p.pane], a.state == .blocked, a.kind == .trust else {
                await fail(c, id: id, kind: .conflict, hint: "pane \(p.pane) shows no folder trust prompt")
                break
            }
            await reply(c, id: id, result: TrustAnswered(dropped: p.choice == .exit ? trustDropped : 0, seq: c.seq))
            a.state = p.choice == .trust ? .working : .exited
            a.kind = nil
            a.trust = nil
            a.at = Fixture.stamp(Date())
            await put(a)
        case HabMethods.speechPrepare.name:
            guard capabilities.contains(speechCapability) else {
                await fail(c, id: id, kind: .unavailable, hint: "this hab has no speech engine")
                break
            }
            speechPrepares += 1
            var status = speechPrepare
            status.seq = c.seq
            await reply(c, id: id, result: status)
        case HabMethods.speechTranscribe.name:
            guard capabilities.contains(speechCapability) else {
                await fail(c, id: id, kind: .unavailable, hint: "this hab has no speech engine")
                break
            }
            guard let p = try? params.decode(SpeechTranscribeParams.self) else {
                await fail(c, id: id, kind: .invalid, hint: "bad speech.transcribe params")
                break
            }
            transcribes.append(p)
            if let f = nextSpeechFailure {
                nextSpeechFailure = nil
                await fail(c, id: id, kind: f.kind, hint: f.hint)
                break
            }
            if speechPrepare.state != .ready && speechPrepare.state != .loading {
                let hint = speechPrepare.progress.map { "downloading \($0)%" } ?? "the model is not downloaded"
                await fail(c, id: id, kind: .unavailable, hint: hint)
                break
            }
            if p.audio.count > SpeechLimits.maxClipBytes {
                await fail(c, id: id, kind: .invalid, hint: "the clip is over 1 MiB")
                break
            }
            let n = transcribes.count - 1
            let text = speechTexts.isEmpty ? "a clip of \(p.audio.count) bytes" : speechTexts[min(n, speechTexts.count - 1)]
            await reply(c, id: id, result: SpeechTranscript(audioMs: 0, decodeMs: 0, seq: c.seq, text: text))
        default:
            await fail(c, id: id, kind: .unavailable, hint: "the fake hab does not serve \(method)")
        }
        return true
    }

    /// The Capcom entry calls, by the real hab's rules (capcom.md 8.4): refs resolve at once,
    /// at most 8 items wait, the same item twice is kept once, and `open` refuses a file.
    private func capcomCall(_ method: String, _ params: JSONValue, _ c: Conn, id: Int) async {
        guard capabilities.contains("capcom") else {
            return await fail(c, id: id, kind: .unavailable, hint: "this hab runs no Capcom")
        }
        if let f = nextCapcomFailure {
            nextCapcomFailure = nil
            return await fail(c, id: id, kind: f.kind, hint: f.hint)
        }
        do {
            switch method {
            case HabMethods.capcomAssign.name:
                // The real hab's rule for `self` (capcom.md 7.3): a counter above every one seen,
                // at least the floor given. A peer label needs a link the fake has none of.
                let p = try decode(CapcomAssignParams.self, params, method)
                capcomAssigns.append(p)
                guard p.host == "self" || p.host == deviceID else {
                    throw Failure(kind: .notFound, hint: "no peer \"\(p.host)\" in this hab's peer list")
                }
                let n = max(p.assignment ?? 0, capcomHome.assignment + 1)
                if capcomHome.home != deviceID { capcomStanding = nil } // a new home starts fresh
                capcomHomeSet = (n, deviceID)
                await reply(c, id: id, result: CapcomAssigned(assignment: n, home: deviceID, seq: c.seq))
                if !holdAssignAnnounce { await announceCatalog() }
            case HabMethods.capcomOpen.name:
                let p = try decode(CapcomOpenParams.self, params, method)
                capcomOpens.append(p)
                // Not the home host (capcom.md 7.6): the real hab names the home, or says to assign one.
                guard let home = capcomHome.home else {
                    throw Failure(kind: .unavailable, hint: "no host runs Capcom yet; assign one with gff-hab capcom assign <peer label|self>")
                }
                guard home == deviceID else {
                    throw Failure(kind: .unavailable, hint: "Capcom runs on \(capcomPeers[home] ?? home) (assignment \(capcomHome.assignment)), not on this host")
                }
                let refs = p.context ?? []
                guard refs.count <= 8 else { throw Failure(kind: .invalid, hint: "at most 8 context items") }
                if refs.contains(where: { $0.kind == .file }) {
                    throw Failure(kind: .invalid, hint: "attach files with capcom.attach after the open")
                }
                let items = try refs.map { try resolve($0, session: nil) }
                var started = false
                if p.new == true || capcomStanding.flatMap({ sessions[$0] }) == nil {
                    capcomCount += 1
                    let sid = "capcom-s\(capcomCount)"
                    let pane = "%c\(capcomCount)"
                    let name = capcomCount == 1 ? "capcom" : "capcom-\(capcomCount)"
                    if !items.isEmpty { _ = try adding(items, to: sid) }
                    capcomStanding = sid
                    started = true
                    if capcomService {
                        // The service session (capcom-service.md 2.1): no pane; its agent state
                        // rides on the engine route.
                        await put(Session(dir: "/state/capcom/engine", id: sid, kind: .capcom, name: name, pane: "", revision: 0, state: .running))
                        await put(Fixture.agent(pane: CapcomThread.engineRoute, harness: "capcom", state: .idle))
                    } else {
                        await put(Session(dir: "/state/capcom/\(capcomCount)", id: sid, kind: .capcom, name: name, pane: pane, revision: 0, state: .running))
                        await put(Fixture.pane(pane, session: sid))
                        await put(Fixture.agent(pane: pane, harness: "claude", state: .idle))
                    }
                } else if let sid = capcomStanding, var s = sessions[sid], s.state == .exited {
                    // The real hab resumes an exited standing conversation (capcom.md 7.6).
                    if !items.isEmpty { _ = try adding(items, to: sid) }
                    s.state = .running
                    s.exit = nil
                    started = true
                    capcomResumes += 1
                    await put(s)
                }
                guard let sid = capcomStanding, let s = sessions[sid] else { throw Failure(kind: .unavailable, hint: "no Capcom session") }
                let list = try adding(items, to: sid)
                capcomWaiting[sid] = list.isEmpty ? nil : list
                await reply(c, id: id, result: CapcomOpened(dir: s.dir ?? "", pane: s.pane ?? "", pending: list.isEmpty ? nil : list,
                                                           seq: c.seq, session: sid, started: started ? true : nil))
            case HabMethods.capcomPending.name:
                // Read-only: it starts, resumes and changes nothing.
                let p = try decode(CapcomPendingParams.self, params, method)
                capcomPendingReads += 1
                try capcomSession(p.session)
                await reply(c, id: id, result: CapcomAttached(pending: capcomWaiting[p.session] ?? [], seq: c.seq, session: p.session))
            case HabMethods.capcomAttach.name:
                let p = try decode(CapcomAttachParams.self, params, method)
                capcomAttaches.append(p)
                try capcomSession(p.session)
                let item = try resolve(CapcomContextRef(host: p.host, kind: p.kind, ref: p.ref), session: p.session)
                let list = try adding([item], to: p.session)
                capcomWaiting[p.session] = list
                await reply(c, id: id, result: CapcomAttached(pending: list, seq: c.seq, session: p.session))
            default:
                let p = try decode(CapcomDetachParams.self, params, method)
                capcomDetaches.append(p)
                try capcomSession(p.session)
                let host = try hostLabel(p.host, kind: .notification)
                var list = capcomWaiting[p.session] ?? []
                guard let i = list.firstIndex(where: { $0.kind == p.kind && $0.ref == p.ref && $0.host == host }) else {
                    throw Failure(kind: .notFound, hint: "no \(p.kind.rawValue) \(p.ref) waits for session \(p.session)")
                }
                list.remove(at: i)
                capcomWaiting[p.session] = list.isEmpty ? nil : list
                await reply(c, id: id, result: CapcomAttached(pending: list, seq: c.seq, session: p.session))
            }
        } catch let f as Failure {
            await fail(c, id: id, kind: f.kind, hint: f.hint)
        } catch {
            await fail(c, id: id, kind: .invalid, hint: "bad \(method) params")
        }
    }

    private func replyCatalog(_ c: Conn, id: Int) async {
        await reply(c, id: id, result: CatalogGetResult(catalog: catalog(), generation: catalogGeneration, seq: c.seq))
    }

    /// A minimal catalog: the host, its device id and, with the capability `capcom`, the
    /// Capcom section (capcom.md 7.6).
    private func catalog() -> Catalog {
        var cap: CatalogCapcom?
        if capabilities.contains("capcom") {
            let mine = sessions.values.filter { $0.kind == .capcom }.sorted { $0.id < $1.id }
                .map { CatalogCapcomSession(harness: "claude", name: $0.name, session: $0.id, standing: $0.id == capcomStanding ? true : nil) }
            if capcomHome.home != nil || !mine.isEmpty {
                cap = CatalogCapcom(assignment: capcomHome.home == nil ? 0 : capcomHome.assignment, home: capcomHome.home, sessions: mine)
            }
        }
        return Catalog(actions: [], agents: [], alwaysOn: alwaysOn ? true : nil, at: Fixture.stamp(Date()), capabilities: [], capcom: cap, deviceId: deviceID,
                       generation: catalogGeneration, harnesses: [], host: hostID, peers: [], projects: [], services: [],
                       system: CatalogSystem(arch: "arm64", cores: 8, os: "darwin"), toolchains: [], watches: [])
    }

    private func decode<T: Decodable>(_ t: T.Type, _ params: JSONValue, _ method: String) throws -> T {
        guard let p = try? params.decode(T.self) else { throw Failure(kind: .invalid, hint: "bad \(method) params") }
        return p
    }

    private func capcomSession(_ id: String) throws {
        // The real hab answers invalid for any session with no Capcom record, unknown ones too.
        guard sessions[id]?.kind == .capcom else { throw Failure(kind: .invalid, hint: "session \(id) is not a Capcom session on this hab") }
    }

    /// The label of the host a ref names: this hab for none, `self`, its host id or device id.
    private func hostLabel(_ host: String?, kind: CapcomContextKind) throws -> String {
        guard let h = host, !h.isEmpty, h != "self", h != hostID, h != "SHA256:fake-" + hostID else { return hostID }
        guard kind == .notification else { throw Failure(kind: .invalid, hint: "only a notification may name another host") }
        guard let label = capcomPeers[h] else { throw Failure(kind: .notFound, hint: "no host \(h) in this hab's peer list") }
        return label
    }

    private func resolve(_ r: CapcomContextRef, session: String?) throws -> CapcomContextItem {
        let host = try hostLabel(r.host, kind: r.kind)
        let title: String
        switch r.kind {
        case .notification:
            // A peer's notice: the fake resolves any id there.
            guard host == hostID else { return CapcomContextItem(host: host, kind: r.kind, ref: r.ref, title: "Notice \(r.ref)") }
            guard let t = noticeTitles[r.ref] else { throw Failure(kind: .notFound, hint: "no notice \(r.ref)") }
            title = t
        case .session:
            guard let s = sessions[r.ref] else { throw Failure(kind: .notFound, hint: "no session \(r.ref)") }
            title = s.name
        case .command:
            guard r.ref.split(separator: ":").count == 2 else { throw Failure(kind: .invalid, hint: "a command ref is <pane>:<command id>") }
            guard let line = capcomCommands[r.ref] else { throw Failure(kind: .notFound, hint: "no command \(r.ref)") }
            title = line
        case .file:
            guard let f = storedFiles[r.ref], f.put.session == session else {
                throw Failure(kind: .notFound, hint: "no attachment \(r.ref) for session \(session ?? "")")
            }
            title = f.put.name
        case .logbook:
            throw Failure(kind: .unavailable, hint: "this hab offers no Logbook")
        default:
            throw Failure(kind: .invalid, hint: "unknown kind \(r.kind.rawValue)")
        }
        return CapcomContextItem(host: host, kind: r.kind, ref: r.ref, title: String(title.prefix(120)))
    }

    /// The waiting list with `items` added, each once; past 8 it is refused and adds nothing.
    private func adding(_ items: [CapcomContextItem], to session: String) throws -> [CapcomContextItem] {
        var list = capcomWaiting[session] ?? []
        for it in items where !list.contains(where: { $0.kind == it.kind && $0.ref == it.ref && $0.host == it.host }) {
            list.append(it)
        }
        guard list.count <= 8 else { throw Failure(kind: .conflict, hint: "8 context items already wait") }
        return list
    }

    /// The attachment methods, by the real hab's rules: runs in order, the hash checked at the end.
    private func attachmentCall(_ method: String, _ params: JSONValue, _ c: Conn, id: Int) async {
        guard capabilities.contains("attachments") else {
            return await fail(c, id: id, kind: .unavailable, hint: "the attachments capability is absent")
        }
        switch method {
        case HabMethods.attachmentPut.name:
            guard let p = try? params.decode(AttachmentPutParams.self) else {
                return await fail(c, id: id, kind: .invalid, hint: "bad attachment.put params")
            }
            guard sessions[p.session] != nil else { return await fail(c, id: id, kind: .notFound, hint: "no session \(p.session)") }
            guard p.size <= 25_000_000 else { return await fail(c, id: id, kind: .invalid, hint: "the limit is 25 MB") }
            let aid = String(format: "%012x", nextAttachment)
            nextAttachment += 1
            let path = "/work/proj/.flightdeck/attachments/\(aid)-\(p.name)"
            if p.size == 0 {
                storedFiles[aid] = (p, Data())
                return await reply(c, id: id, result: AttachmentStored(chunk: attachmentChunk, done: true, id: aid, path: path, seq: c.seq))
            }
            uploads[aid] = (p, Data())
            await reply(c, id: id, result: AttachmentStored(chunk: attachmentChunk, id: aid, path: path, seq: c.seq))
        case HabMethods.attachmentWrite.name:
            guard let p = try? params.decode(AttachmentWriteParams.self) else {
                return await fail(c, id: id, kind: .invalid, hint: "bad attachment.write params")
            }
            attachmentWrites += 1
            if attachmentWritesBeforeFailures > 0 {
                attachmentWritesBeforeFailures -= 1
            } else if attachmentWriteFailures > 0 {
                attachmentWriteFailures -= 1
                return await fail(c, id: id, kind: .unavailable, hint: "the write broke off")
            }
            if let done = storedFiles[p.id] {
                return await reply(c, id: id, result: AttachmentWritten(done: true, id: p.id, received: done.data.count, seq: c.seq))
            }
            guard var u = uploads[p.id] else { return await fail(c, id: id, kind: .notFound, hint: "no upload \(p.id)") }
            if attachmentStall {
                return await reply(c, id: id, result: AttachmentWritten(done: false, id: p.id, received: u.data.count, seq: c.seq))
            }
            guard p.offset == u.data.count else {
                return await fail(c, id: id, kind: .conflict, hint: "upload \(p.id) has \(u.data.count) bytes")
            }
            guard !p.data.isEmpty, p.data.count <= attachmentChunk, u.data.count + p.data.count <= Int(u.put.size) else {
                return await fail(c, id: id, kind: .invalid, hint: "bad run")
            }
            u.data.append(p.data)
            if u.data.count < Int(u.put.size) {
                uploads[p.id] = u
                return await reply(c, id: id, result: AttachmentWritten(done: false, id: p.id, received: u.data.count, seq: c.seq))
            }
            uploads[p.id] = nil
            guard AttachmentUpload.sha256(u.data) == u.put.sha256 else {
                return await fail(c, id: id, kind: .invalid, hint: "the hash does not match")
            }
            storedFiles[p.id] = u
            await reply(c, id: id, result: AttachmentWritten(done: true, id: p.id, received: u.data.count, seq: c.seq))
        default:
            guard let p = try? params.decode(AttachmentGetParams.self) else {
                return await fail(c, id: id, kind: .invalid, hint: "bad attachment.get params")
            }
            guard let f = storedFiles[p.id] else { return await fail(c, id: id, kind: .notFound, hint: "no attachment \(p.id)") }
            if p.thumbnail == true {
                guard f.put.type.hasPrefix("image/") else {
                    return await fail(c, id: id, kind: .invalid, hint: "no thumbnail for \(p.id) (\(f.put.type))")
                }
                // The fake's thumbnail is the file itself.
                return await reply(c, id: id, result: AttachmentData(data: f.data, eof: true, id: p.id, name: f.put.name, seq: c.seq, size: f.data.count, type: "image/jpeg"))
            }
            attachmentGetLimits.append(p.limit)
            let off = min(p.offset ?? 0, f.data.count)
            let end = min(off + min(p.limit ?? 256 << 10, 1 << 20), f.data.count)
            await reply(c, id: id, result: AttachmentData(data: f.data.subdata(in: off..<end), eof: end == f.data.count, id: p.id,
                                                         name: f.put.name, offset: off, seq: c.seq, size: f.data.count, type: f.put.type))
        }
    }

    /// The content methods, from `setContent`. Like the real hab, only listed ids are read.
    private func contentCall(_ method: String, _ params: JSONValue, _ c: Conn, id: Int) async {
        guard capabilities.contains("content") else {
            await fail(c, id: id, kind: .unavailable, hint: "the content capability is absent")
            return
        }
        func listed(_ session: String, _ item: String) -> ContentItem? {
            contentItems[session]?.first { $0.id == item }
        }
        switch method {
        case HabMethods.contentList.name:
            guard let p = try? params.decode(ContentListParams.self) else {
                return await fail(c, id: id, kind: .invalid, hint: "bad content.list params")
            }
            contentLists += 1
            let items = contentItems[p.session] ?? []
            if let h = contentListHook { await h(contentLists) }
            await reply(c, id: id, result: ContentList(items: items, more: false, seq: c.seq))
        case HabMethods.contentRead.name:
            guard let p = try? params.decode(ContentReadParams.self) else {
                return await fail(c, id: id, kind: .invalid, hint: "bad content.read params")
            }
            contentReads += 1
            if contentUnavailable > 0 {
                contentUnavailable -= 1
                return await fail(c, id: id, kind: .unavailable, hint: "4 content calls already run on this connection")
            }
            if let h = contentReadHook { await h(p) }
            guard let it = listed(p.session, p.id), let f = contentFiles[it.id] else {
                return await fail(c, id: id, kind: .notFound, hint: "no content \(p.id) in session \(p.session)")
            }
            let off = p.offset ?? 0
            guard off <= f.data.count else {
                return await fail(c, id: id, kind: .invalid, hint: "offset past the end")
            }
            let n = min(p.limit ?? (256 << 10), contentChunk, f.data.count - off)
            let chunk = f.data.subdata(in: off..<(off + n))
            await reply(c, id: id, result: ContentData(data: chunk, eof: off + n >= f.data.count, id: it.id, modified: f.modified,
                                                       offset: off, seq: c.seq, size: f.data.count, type: it.type))
        default:
            guard let p = try? params.decode(ContentDiffParams.self) else {
                return await fail(c, id: id, kind: .invalid, hint: "bad content.diff params")
            }
            guard let it = listed(p.session, p.id) else {
                return await fail(c, id: id, kind: .notFound, hint: "no content \(p.id) in session \(p.session)")
            }
            let d = contentDiffs[it.id]
            await reply(c, id: id, result: ContentDiff(diff: d ?? "", id: it.id, path: it.path, seq: c.seq, source: d == nil ? .none : .git))
        }
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
            confirmations: capabilities.contains(ConfirmBook.capability) && confirms.contains(where: \.asks) ? confirms.filter(\.asks) : nil,
            groups: capabilities.contains(GroupCenter.capability) && !groupBook.isEmpty ? groupBook.values.sorted { $0.id < $1.id } : nil,
            host: HostInfo(id: hostID),
            panes: panes.values.sorted { $0.id < $1.id },
            projects: projects.values.sorted { $0.id < $1.id },
            rounds: capabilities.contains(RoundCenter.capability) ? roundBook.list() : nil,
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
