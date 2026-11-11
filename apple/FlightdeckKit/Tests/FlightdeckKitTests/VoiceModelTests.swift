import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// A microphone and speaker that tests drive.
@MainActor
final class FakeVoiceIO: VoiceInput, VoiceSpeaker {
    let events: AsyncStream<VoiceInputEvent>
    private let sink: AsyncStream<VoiceInputEvent>.Continuation
    var hasLiveWords = true
    var startError: Error?
    private(set) var started = false
    private(set) var stopped = false
    private(set) var mutedNow = false
    private(set) var turnsBegun = 0
    /// What the next `endTurn` returns.
    var nextTurn = (clip: SpeechClip(samples: Array(repeating: 0.2, count: 16_000)), live: "")
    /// Everything spoken, in order.
    private(set) var spoken: [String] = []
    private(set) var tones = 0
    /// False: `speak` waits until `finishSpeaking` or `stopSpeaking`.
    var autoFinish = true
    private var pending: CheckedContinuation<Bool, Never>?

    init() {
        (events, sink) = AsyncStream.makeStream(of: VoiceInputEvent.self)
    }

    func emit(_ e: VoiceInputEvent) { sink.yield(e) }

    func start() async throws {
        if let startError { throw startError }
        started = true
    }
    func stop() {
        stopped = true
        stopSpeaking()
        sink.finish()
    }
    func setMuted(_ muted: Bool) { mutedNow = muted }
    func beginTurn() { turnsBegun += 1 }
    func endTurn() async -> (clip: SpeechClip, live: String) { nextTurn }

    var isSpeaking: Bool { pending != nil }
    func speak(_ text: String) async -> Bool {
        spoken.append(text)
        if autoFinish {
            await Task.yield()
            return true
        }
        return await withCheckedContinuation { pending = $0 }
    }
    func finishSpeaking() {
        let p = pending
        pending = nil
        p?.resume(returning: true)
    }
    func stopSpeaking() {
        let p = pending
        pending = nil
        p?.resume(returning: false)
    }
    func tone() { tones += 1 }
    func chime() {}
}

@MainActor
final class VoiceModelTests: XCTestCase {
    struct Bench {
        let fleet: Fleet
        let hab: FakeHab
        let chat: ChatModel
        let io: FakeVoiceIO
        let model: VoiceModel
        let conn: HabConnection
    }

    private func setup(state: AgentStatus = .idle, kind: BlockedKind? = nil, request: PermissionRequest? = nil,
                       speech: Bool = true, settings: VoiceSettings = VoiceSettings(), parakeet: ParakeetHost? = nil) async -> Bench {
        let hab = await sampleHab()
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: state, kind: kind, request: request))
        await hab.setChat(session: "s1", [ChatMessage(at: "2026-11-01T10:00:00Z", id: "m0", kind: .agent, session: "s1", text: "Earlier reply.")])
        await hab.setCapability(speechCapability, speech)
        let fleet = Fleet()
        let conn = HabConnection(id: "h", transport: hab.transport(), timing: fastTiming())
        fleet.add(conn, label: "bench")
        await fleet.setActive(true)
        await eventually("online") { fleet.board.habs.first?.state.status == .online }
        let chat = fleet.chat(for: SessionTarget(habID: "h", session: "s1", pane: "%1"))
        chat.readBackWindow = .seconds(5)
        let io = FakeVoiceIO()
        let store = VoiceSettingsStore(defaults: emptyDefaults())
        store.value = settings
        let kit = VoiceKit(settings: store, parakeet: parakeet ?? ParakeetHost(runtime: nil), liveWords: true)
        kit.makeAudio = { _ in io }
        var timing = VoiceModel.Timing()
        timing.tick = .milliseconds(20)
        timing.replyWait = .milliseconds(500)
        let model = VoiceModel(chat: chat, engines: kit.engines(connection: conn, session: "s1", holder: "voice"),
                               input: io, speaker: io, settings: settings, timing: timing) { params in
            _ = try await conn.call(HabMethods.approvalAnswer, params)
        }
        return Bench(fleet: fleet, hab: hab, chat: chat, io: io, model: model, conn: conn)
    }

    /// The owner speaks: words, then a tap ends the turn.
    private func say(_ b: Bench, live: String) async {
        b.io.nextTurn.live = live
        let begun = b.io.turnsBegun
        b.io.emit(.words(live, at: 0))
        // The model began the turn. Its phase alone cannot say so: while it waits for an answer,
        // it is already answering before the words arrive, and a turn it has not begun does not end.
        await eventually("hearing") { b.io.turnsBegun > begun }
        await b.model.endTurn()
    }

    func testSpokenTurnGoesThroughTheSendPathWithTheSummaryRequest() async {
        let b = await setup()
        await b.hab.setTranscripts(["run the link tests"])
        await b.model.start()
        XCTAssertTrue(b.io.started)
        XCTAssertEqual(b.model.phase, .listening)
        await eventually("hab engine ready") { b.model.engines.status[.hab] == .ready }

        await say(b, live: "run the link test")
        await eventually("sent") { await b.hab.sends.count == 1 }
        let sends = await b.hab.sends
        XCTAssertEqual(sends[0].origin, .dictated)
        XCTAssertTrue(sends[0].text.hasPrefix("run the link tests\n\n"))
        XCTAssertTrue(sends[0].text.contains(SpokenText.summaryMarker))
        XCTAssertEqual(b.model.engineLabel, SpeechEngineKind.hab.label)
        XCTAssertEqual(b.model.liveText, "run the link tests")
        XCTAssertEqual(b.model.phase, .waiting)
        let clips = await b.hab.transcribes
        XCTAssertEqual(clips.first?.session, "s1")

        // The agent works, then ends: only the final text is read, trimmed.
        await b.hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .working))
        await eventually("working") { b.chat.context.agentStatus == .working }
        await b.hab.putChat(ChatMessage(at: "2026-11-01T10:01:00Z", id: "m1", kind: .user, session: "s1", text: sends[0].text))
        await b.hab.putChat(ChatMessage(at: "2026-11-01T10:01:01Z", id: "m2", kind: .agent, session: "s1", text: "Running them."))
        await b.hab.putChat(ChatMessage(at: "2026-11-01T10:01:02Z", id: "m3", kind: .tool, session: "s1", tool: ChatTool(name: "Bash", status: .done, target: "go test ./internal/link/")))
        await b.hab.putChat(ChatMessage(at: "2026-11-01T10:01:03Z", id: "m4", kind: .agent, session: "s1",
                                        text: "All pass.\n```\nok link 0.3s\n```\nSpoken summary: The link tests pass. Nothing else changed."))
        await b.hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .done))
        await eventually("spoken") { b.io.spoken.contains("The link tests pass. Nothing else changed.") }
        await eventually("listening again") { b.model.phase == .listening }
        XCTAssertFalse(b.io.spoken.contains { $0.contains("Running them") })
        await b.model.end()
        XCTAssertTrue(b.io.stopped)
        await b.fleet.setActive(false)
    }

    func testFallbackToAppleWhenTheHabHasNoSpeechAndTheFooterSaysSo() async {
        let b = await setup(speech: false)
        await b.model.start()
        await eventually("hab skipped") { b.model.engines.skipped[.hab] != nil }
        await say(b, live: "check the build")
        await eventually("sent") { await b.hab.sends.count == 1 }
        let sends = await b.hab.sends
        XCTAssertTrue(sends[0].text.hasPrefix("check the build"))
        XCTAssertEqual(b.model.engineLabel, SpeechEngineKind.apple.label)
        let n = await b.hab.transcribes.count
        XCTAssertEqual(n, 0)
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testDownloadingHabFallsBackPerTurn() async {
        let b = await setup()
        await b.hab.setSpeechPrepare(status(.downloading, progress: 40))
        await b.model.start()
        await eventually("status") { b.model.engines.status[.hab] == .preparing("downloading 40%") }
        await say(b, live: "first words")
        await eventually("sent") { await b.hab.sends.count == 1 }
        XCTAssertEqual(b.model.engineLabel, SpeechEngineKind.apple.label)
        XCTAssertEqual(b.model.engines.lastFallback, "Parakeet on the hab: downloading 40%")
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testSilenceEndsTheTurn() async {
        var s = VoiceSettings()
        s.silence = 0.15
        s.askSummary = false
        let b = await setup(speech: false, settings: s)
        await b.model.start()
        b.io.nextTurn.live = "hello there"
        b.io.emit(.words("hello there", at: 0))
        await eventually("sent after silence") { await b.hab.sends.count == 1 }
        let sends = await b.hab.sends
        XCTAssertEqual(sends[0].text, "hello there")
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testBargeInStopsPlayback() async {
        let b = await setup(speech: false)
        b.io.autoFinish = false
        await b.model.start()
        // "repeat" with nothing read yet speaks a line we can talk over.
        b.io.nextTurn.live = "repeat"
        b.io.emit(.words("repeat", at: 0))
        await eventually("hearing") { b.model.phase == .hearing }
        let t = Task { await b.model.endTurn() }
        await eventually("speaking") { b.model.phase == .speaking }
        // Loud speech over it: a barge-in stops playback and starts a turn.
        for i in 0..<20 {
            b.io.emit(.level(-10, at: Double(i) * 0.02))
            try? await Task.sleep(for: .milliseconds(20))
            if b.model.phase == .hearing { break }
        }
        await eventually("barged in") { b.model.phase == .hearing }
        XCTAssertFalse(b.io.isSpeaking)
        await t.value
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testSpokenCommands() async {
        let b = await setup(state: .working, speech: false)
        await b.model.start()
        XCTAssertEqual(b.model.phase, .waiting)
        await say(b, live: "Stop.")
        await eventually("interrupt went out") { await b.hab.inputs.count == 1 || b.chat.notice != nil }
        let sends = await b.hab.sends
        XCTAssertTrue(sends.isEmpty, "a command is not a message")
        // A sentence with the word in it is a message, sent after this turn.
        await say(b, live: "stop the server please")
        XCTAssertEqual(b.chat.outgoing.last?.status, .queued)
        XCTAssertEqual(b.io.spoken.last, "Queued.")
        await say(b, live: "end voice")
        XCTAssertTrue(b.model.isEnded)
        await b.fleet.setActive(false)
    }

    func testCommandsOffSendsTheWord() async {
        var s = VoiceSettings()
        s.spokenCommands = false
        s.askSummary = false
        let b = await setup(speech: false, settings: s)
        await b.model.start()
        await say(b, live: "stop")
        await eventually("sent") { await b.hab.sends.count == 1 }
        let sends = await b.hab.sends
        XCTAssertEqual(sends[0].text, "stop")
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testConfirmBeforeSend() async {
        var s = VoiceSettings()
        s.confirmBeforeSend = true
        s.askSummary = false
        let b = await setup(speech: false, settings: s)
        await b.model.start()
        await say(b, live: "deploy to staging")
        XCTAssertEqual(b.model.phase, .confirming("deploy to staging"))
        let none = await b.hab.sends
        XCTAssertTrue(none.isEmpty)
        await b.model.tap()
        await eventually("sent") { await b.hab.sends.count == 1 }
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testApprovalByVoiceReadsBackAndAcceptsOnlyAllowOrDeny() async {
        let req = PermissionRequest(command: "go test ./...", id: "r1", title: "Run go test?", tool: "Bash")
        let b = await setup(state: .blocked, kind: .permission, request: req, speech: false)
        await b.model.start()
        await eventually("asked") { b.model.phase == .answering }
        XCTAssertTrue(b.io.spoken.first?.hasPrefix("Run go test? go test") == true, "\(b.io.spoken)")
        XCTAssertTrue(b.io.spoken.first?.hasSuffix("Allow or deny?") == true)
        XCTAssertTrue(b.model.requestMayAllow)
        XCTAssertFalse(b.io.mutedNow, "listening again after asking")
        // Not a clear answer: it asks once more.
        await say(b, live: "maybe later")
        XCTAssertEqual(b.io.spoken.last, "Allow or deny?")
        let none = await b.hab.answers
        XCTAssertTrue(none.isEmpty)
        await say(b, live: "Allow.")
        await eventually("answered") { await b.hab.answers.count == 1 }
        let a = await b.hab.answers[0]
        XCTAssertEqual(a.choice, .allow)
        XCTAssertEqual(a.request, "r1")
        XCTAssertEqual(a.via, .voice)
        await eventually("said") { b.io.spoken.last == "Allowed." }
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testTwoUnclearAnswersWaitForATap() async {
        let req = PermissionRequest(command: "rm -rf build", id: "r2", title: "Run rm?", tool: "Bash")
        let b = await setup(state: .blocked, kind: .permission, request: req, speech: false)
        await b.model.start()
        await eventually("asked") { b.model.phase == .answering }
        await say(b, live: "maybe")
        await eventually("asked again") { b.model.phase == .answering }
        await say(b, live: "what")
        await eventually("tap") { b.model.phase == .tapToAnswer }
        XCTAssertEqual(b.io.spoken.last, "Tap Allow or Deny.")
        await b.model.answerByTap(false)
        await eventually("denied") { await b.hab.answers.first?.choice == .deny }
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testALongCommandCanOnlyBeDeniedByVoice() async {
        let long = Array(repeating: "arg", count: 20).joined(separator: " ")
        let req = PermissionRequest(command: "tool " + long, id: "r3", title: "Run tool?", tool: "Bash")
        let b = await setup(state: .blocked, kind: .permission, request: req, speech: false)
        await b.model.start()
        await eventually("asked") { b.model.phase == .answering }
        XCTAssertFalse(b.model.requestMayAllow)
        XCTAssertTrue(b.io.spoken.first?.hasSuffix("This one can be allowed only on screen. Say deny, or tap.") == true)
        await say(b, live: "allow")
        await eventually("refused") { b.model.phase == .tapToAnswer }
        let none = await b.hab.answers
        XCTAssertTrue(none.isEmpty)
        await say(b, live: "deny")
        await eventually("denied") { await b.hab.answers.first?.choice == .deny }
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testAnsweredElsewhereStopsAsking() async {
        let req = PermissionRequest(command: "ls", id: "r4", title: "Run ls?", tool: "Bash")
        let b = await setup(state: .blocked, kind: .permission, request: req, speech: false)
        await b.model.start()
        await eventually("asked") { b.model.phase == .answering }
        await b.hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .working))
        await eventually("dropped") { b.model.request == nil }
        XCTAssertEqual(b.model.phase, .waiting)
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testTurnStoppedAndDone() async {
        let b = await setup(state: .working, speech: false)
        await b.model.start()
        await b.hab.putChat(ChatMessage(at: "2026-11-01T10:01:00Z", id: "u1", kind: .user, session: "s1", text: "go"))
        await b.hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .error))
        await eventually("stopped") { b.io.spoken.last == "The turn stopped." }
        await b.hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .working))
        await eventually("working") { b.chat.context.agentStatus == .working }
        await b.hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .done))
        await eventually("done") { b.io.spoken.last == "Done." }
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testOfflineReadsNothingUntilTheTurnEndIsKnown() async {
        let b = await setup(state: .working, speech: false)
        await b.hab.putChat(ChatMessage(at: "2026-11-01T10:01:00Z", id: "u1", kind: .user, session: "s1", text: "go"))
        await b.model.start()
        await b.hab.dropConnections()
        await b.hab.setSilent(true)
        await eventually("lost") { b.model.notice == "Lost bench" }
        await b.hab.setSilent(false)
        await b.hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .done))
        await eventually("reconnected and read", timeout: 10) { b.io.spoken.last == "Done." }
        XCTAssertEqual(b.io.spoken.filter { $0 == "Done." }.count, 1)
        await b.model.end()
        await b.fleet.setActive(false)
    }

    /// A model over one slow engine, so a test can act while a turn is transcribed.
    private func slowModel(_ b: Bench, text: String, settings: VoiceSettings = VoiceSettings()) -> (VoiceModel, FakeEngine) {
        let engine = FakeEngine(.apple, [.success(text)])
        engine.delay = .milliseconds(300)
        var t = VoiceModel.Timing()
        t.tick = .milliseconds(20)
        t.replyWait = .milliseconds(500)
        let conn = b.conn
        let m = VoiceModel(chat: b.chat, engines: SpeechEngines([engine]), input: b.io, speaker: b.io, settings: settings, timing: t) { params in
            _ = try await conn.call(HabMethods.approvalAnswer, params)
        }
        return (m, engine)
    }

    func testARequestReplacedDuringTheTranscriptionIsNotAnswered() async {
        let r1 = PermissionRequest(command: "ls", id: "r1", title: "Run ls?", tool: "Bash")
        let b = await setup(state: .blocked, kind: .permission, request: r1, speech: false)
        let (m, engine) = slowModel(b, text: "allow")
        // The transcription ends only once r2 reached the model. Ending before that, the allow
        // goes to r1, the request the owner heard, which is right: the hab refuses it when r1
        // was answered elsewhere, and it never reaches r2.
        engine.hold = { await eventually("r2 reached the model") { m.request?.id == "r2" } }
        await m.start()
        await eventually("asked r1") { m.phase == .answering }
        b.io.emit(.words("allow", at: 0))
        let turn = Task { await m.endTurn() }
        await eventually("transcribing") { m.phase == .transcribing }
        // r1 is answered elsewhere and r2 arrives while the owner's "allow" is transcribed.
        let r2 = PermissionRequest(command: "rm -rf build", id: "r2", title: "Run rm?", tool: "Bash")
        await b.hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .permission, request: r2))
        await turn.value
        await eventually("r2 read back") { m.request?.id == "r2" && m.phase == .answering }
        let answers = await b.hab.answers
        XCTAssertTrue(answers.isEmpty, "the allow was for r1, never r2: \(answers)")
        XCTAssertTrue(b.io.spoken.contains { $0.hasPrefix("Run rm?") })
        await m.end()
        await b.fleet.setActive(false)
    }

    func testALoneAllowSpokenBeforeTheRequestArrivesNeverAnswers() async {
        let b = await setup(speech: false)
        let (m, _) = slowModel(b, text: "allow")
        await m.start()
        b.io.emit(.words("allow", at: 0))
        await eventually("hearing") { m.phase == .hearing }
        let turn = Task { await m.endTurn() }
        await eventually("transcribing") { m.phase == .transcribing }
        let r1 = PermissionRequest(command: "ls", id: "r1", title: "Run ls?", tool: "Bash")
        await b.hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .permission, request: r1))
        await turn.value
        await eventually("read back") { m.phase == .answering }
        try? await Task.sleep(for: .milliseconds(100))
        let answers = await b.hab.answers
        XCTAssertTrue(answers.isEmpty)
        await m.end()
        await b.fleet.setActive(false)
    }

    func testSaySendWhileConfirming() async {
        var s = VoiceSettings()
        s.confirmBeforeSend = true
        s.askSummary = false
        let b = await setup(speech: false, settings: s)
        await b.model.start()
        await say(b, live: "deploy to staging")
        XCTAssertEqual(b.model.phase, .confirming("deploy to staging"))
        // Other words replace the waiting text.
        await say(b, live: "deploy to production")
        XCTAssertEqual(b.model.phase, .confirming("deploy to production"))
        await say(b, live: "Send.")
        await eventually("sent") { await b.hab.sends.count == 1 }
        let sends = await b.hab.sends
        XCTAssertEqual(sends[0].text, "deploy to production")
        XCTAssertNil(b.model.pendingSend)
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testEndDuringATranscriptionSendsNothing() async {
        let b = await setup(speech: false)
        let (m, _) = slowModel(b, text: "run the tests")
        await m.start()
        b.io.emit(.words("run the tests", at: 0))
        await eventually("hearing") { m.phase == .hearing }
        let turn = Task { await m.endTurn() }
        await eventually("transcribing") { m.phase == .transcribing }
        await m.end()
        await turn.value
        try? await Task.sleep(for: .milliseconds(100))
        let sends = await b.hab.sends
        XCTAssertTrue(sends.isEmpty)
        XCTAssertTrue(m.isEnded, "nothing reopens an ended model")
        await b.fleet.setActive(false)
    }

    func testARouteChangeWhileHearingEndsTheTurn() async {
        var s = VoiceSettings()
        s.askSummary = false
        let b = await setup(speech: false, settings: s)
        await b.model.start()
        b.io.nextTurn.live = "check the logs"
        b.io.emit(.words("check the logs", at: 0))
        await eventually("hearing") { b.model.phase == .hearing }
        b.io.emit(.routeChanged)
        await eventually("sent") { await b.hab.sends.count == 1 }
        await eventually("not stuck") { b.model.phase == .waiting || b.model.phase == .listening }
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testAFullTurnEndsAndIsSent() async {
        var s = VoiceSettings()
        s.askSummary = false
        let b = await setup(speech: false, settings: s)
        await b.model.start()
        b.io.nextTurn.live = "a very long description"
        b.io.emit(.words("a very long description", at: 0))
        await eventually("hearing") { b.model.phase == .hearing }
        b.io.emit(.turnFull)
        await eventually("sent") { await b.hab.sends.count == 1 }
        XCTAssertEqual(b.model.notice, "The turn reached 120 s and ended there.")
        b.io.emit(.liveWords("Downloading speech for live words"))
        await eventually("live words note") { b.model.liveWordsNote == "Downloading speech for live words" }
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testAnInterimTextIsNotReadAsTheReply() async {
        let b = await setup(state: .working, speech: false)
        await b.hab.putChat(ChatMessage(at: "2026-11-01T10:01:00Z", id: "u1", kind: .user, session: "s1", text: "go"))
        await b.model.start()
        await b.hab.putChat(ChatMessage(at: "2026-11-01T10:01:01Z", id: "a1", kind: .agent, session: "s1", text: "Let me look at the file."))
        await b.hab.putChat(ChatMessage(at: "2026-11-01T10:01:02Z", id: "t1", kind: .tool, session: "s1", tool: ChatTool(name: "Read", status: .done, target: "a.go")))
        // The hook's done comes before the reader has the final text.
        await b.hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .done))
        try? await Task.sleep(for: .milliseconds(150))
        await b.hab.putChat(ChatMessage(at: "2026-11-01T10:01:03Z", id: "a2", kind: .agent, session: "s1", text: "The file is fine."))
        await eventually("final read") { b.io.spoken.last == "The file is fine." }
        XCTAssertFalse(b.io.spoken.contains("Let me look at the file."))
        await b.model.end()
        await b.fleet.setActive(false)
    }

    func testWaitToneWhileWorking() async {
        let b = await setup(state: .working, speech: false)
        var t = VoiceModel.Timing()
        t.tick = .milliseconds(10)
        t.waitTone = 0.05
        let m = VoiceModel(chat: b.chat, engines: SpeechEngines([AppleSpeechEngine(supported: true)]), input: b.io, speaker: b.io,
                           settings: VoiceSettings(), timing: t) { _ in }
        await m.start()
        await eventually("tones") { b.io.tones >= 2 }
        await m.end()
        await b.fleet.setActive(false)
    }

    func testIdleLimitEndsVoiceMode() async {
        let b = await setup(speech: false)
        var t = VoiceModel.Timing()
        t.tick = .milliseconds(10)
        t.idleLimit = 0.1
        var reason: String?
        let m = VoiceModel(chat: b.chat, engines: SpeechEngines([AppleSpeechEngine(supported: true)]), input: b.io, speaker: b.io,
                           settings: VoiceSettings(), timing: t) { _ in }
        m.onEnd = { reason = $0 }
        await m.start()
        await eventually("ended") { m.isEnded }
        XCTAssertEqual(reason, "Ended after 10 minutes with no speech.")
        XCTAssertTrue(b.io.stopped)
        await b.fleet.setActive(false)
    }

    func testNoEngineOnEndsAtOnce() async {
        var s = VoiceSettings()
        s.habEngine = false
        s.appleEngine = false
        let b = await setup(settings: s)
        await b.model.start()
        XCTAssertEqual(b.model.phase, .ended("No speech engine is on. Turn one on in Settings."))
        XCTAssertFalse(b.io.started)
        await b.fleet.setActive(false)
    }

    func testParakeetIsHeldWhileVoiceModeIsOpen() async {
        let rt = FakeRuntime()
        let host = ParakeetHost(runtime: rt, offered: true)
        var s = VoiceSettings()
        s.habEngine = false
        s.parakeetEngine = true
        let b = await setup(speech: false, settings: s, parakeet: host)
        await b.model.start()
        await eventually("loaded") { rt.loaded }
        await say(b, live: "apple heard this")
        await eventually("sent") { await b.hab.sends.count == 1 }
        XCTAssertEqual(b.model.engineLabel, SpeechEngineKind.parakeet.label)
        await b.model.end()
        XCTAssertFalse(rt.loaded, "voice mode's end unloads the model")
        await b.fleet.setActive(false)
    }

    func testVoiceHoldKeepsTheConnectionInTheBackground() async {
        let b = await setup(speech: false)
        b.fleet.holdForVoice("h", true)
        await b.fleet.setActive(false)
        try? await Task.sleep(for: .milliseconds(100))
        let status = await b.conn.state.status
        XCTAssertEqual(status, .online, "voice mode keeps its hab")
        b.fleet.holdForVoice("h", false)
        await eventually("closed after the release") { await b.conn.state.status == .stopped }
    }
}
