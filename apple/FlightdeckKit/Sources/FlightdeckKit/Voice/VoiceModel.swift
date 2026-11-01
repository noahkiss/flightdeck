import Foundation
import HabAPI
import Observation

/// Voice mode for one session (app.md 13): listen, send, wait, speak, and
/// approvals by voice. It drives the session's `ChatModel`, so a spoken turn
/// takes the normal send path, and reads the chat to know what to say.
@MainActor
@Observable
public final class VoiceModel {
    public enum Phase: Equatable, Sendable {
        case starting
        /// Waiting for the owner to speak.
        case listening
        /// The owner is speaking.
        case hearing
        /// The turn ended; an engine writes its text.
        case transcribing
        /// "Confirm before send": the owner says "send" or taps.
        case confirming(String)
        /// The agent works on the turn.
        case waiting
        case speaking
        /// Reading a permission request aloud; not listening.
        case asking
        /// Listening for "allow" or "deny".
        case answering
        /// No clear answer twice: the owner taps.
        case tapToAnswer
        case ended(String?)
    }

    /// Timings, short in tests.
    public struct Timing: Sendable {
        public var tick: Duration = .milliseconds(100)
        /// How long a turn end waits for the reply to reach the chat.
        public var replyWait: Duration = .seconds(3)
        public var waitTone: Double = VoiceSettings.waitTone
        public var idleLimit: Double = VoiceSettings.idleLimit
        public init() {}
    }

    public private(set) var phase: Phase = .starting
    /// The words of the current turn as heard, then the engine's text.
    public private(set) var liveText = ""
    /// The level of the microphone, in dBFS.
    public private(set) var level: Float = -160
    public private(set) var muted = false
    /// A short line under the transcript: "Queued", "Lost <hab>", no engine wrote text.
    public private(set) var notice: String?
    /// The latest tool line while the agent works.
    public private(set) var toolLine: String?
    /// The request being answered by voice.
    public private(set) var request: PermissionRequest?
    /// Whether voice may allow `request` (app.md 13.3).
    public private(set) var requestMayAllow = false
    /// What was read last, for "repeat".
    public private(set) var lastSpoken: String?

    public let chat: ChatModel
    public let engines: SpeechEngines
    @ObservationIgnored private let input: VoiceInput
    @ObservationIgnored private let speaker: VoiceSpeaker
    @ObservationIgnored private let settings: VoiceSettings
    @ObservationIgnored private let timing: Timing
    @ObservationIgnored private let answer: (ApprovalAnswerParams) async throws -> Void
    /// Runs once when voice mode ends, for the view to close.
    @ObservationIgnored public var onEnd: (@MainActor (String?) -> Void)?

    @ObservationIgnored private var detector: TurnDetector
    @ObservationIgnored private var tasks: [Task<Void, Never>] = []
    @ObservationIgnored private var lastStatus: AgentStatus?
    @ObservationIgnored private var lastSpokenMessage: String?
    @ObservationIgnored private var askedRequest: String?
    @ObservationIgnored private var answerAttempts = 0
    @ObservationIgnored private var lastActivity: Double = 0
    @ObservationIgnored private var lastTone: Double = 0
    @ObservationIgnored private var interrupted = false
    @ObservationIgnored private let started = ContinuousClock.now

    public init(chat: ChatModel, engines: SpeechEngines, input: VoiceInput, speaker: VoiceSpeaker, settings: VoiceSettings,
                timing: Timing = Timing(), answer: @escaping (ApprovalAnswerParams) async throws -> Void) {
        self.chat = chat
        self.engines = engines
        self.input = input
        self.speaker = speaker
        self.settings = settings
        self.timing = timing
        self.answer = answer
        self.detector = TurnDetector(silence: settings.silence, threshold: settings.threshold)
    }

    /// The engine that wrote the last text, for the footer.
    public var engineLabel: String? { engines.lastEngine?.label }

    public var isEnded: Bool {
        if case .ended = phase { return true }
        return false
    }

    private var agentWorking: Bool { chat.context.online && chat.context.agentStatus == .working }

    private func now() -> Double {
        let d = ContinuousClock.now - started
        return Double(d.components.seconds) + Double(d.components.attoseconds) / 1e18
    }

    // MARK: - Lifecycle

    /// Opens voice mode: warms the engines up, starts the microphone, listens.
    public func start() async {
        guard phase == .starting else { return }
        chat.start()
        lastStatus = chat.context.online ? chat.context.agentStatus : nil
        lastSpokenMessage = SpokenText.finalMessage(chat.messages)?.id
        observeChat()
        if engines.engines.isEmpty {
            await end("No speech engine is on. Turn one on in Settings.")
            return
        }
        let prep = Task { await self.engines.prepare() }
        tasks.append(prep)
        do {
            try await input.start()
        } catch {
            await end(SpeechEngines.describe(error))
            return
        }
        guard !isEnded else { return }
        lastActivity = now()
        tasks.append(Task { [weak self, input] in
            for await e in input.events {
                guard let self else { return }
                self.handle(e)
            }
        })
        tasks.append(Task { [weak self, timing] in
            while !Task.isCancelled {
                try? await Task.sleep(for: timing.tick)
                self?.tick()
            }
        })
        phase = agentWorking ? .waiting : .listening
        chatChanged()
    }

    /// Ends voice mode: stops speech and the microphone, closes the engines.
    public func end(_ reason: String? = nil) async {
        guard !isEnded else { return }
        phase = .ended(reason)
        for t in tasks { t.cancel() }
        tasks = []
        speaker.stopSpeaking()
        input.stop()
        await engines.close()
        chat.stop()
        onEnd?(reason)
    }

    public func setMuted(_ m: Bool) {
        muted = m
        input.setMuted(m)
        detector.reset()
        if m, phase == .hearing { phase = agentWorking ? .waiting : .listening }
        if m { level = -160 }
    }

    /// A tap on the view: stops speech, ends a turn, or confirms a send.
    public func tap() async {
        switch phase {
        case .speaking:
            speaker.stopSpeaking()
        case .hearing:
            await endTurn()
        case .confirming(let text):
            await deliver(text)
        default:
            break
        }
    }

    /// Allow or Deny from the large approval card.
    public func answerByTap(_ allow: Bool) async {
        await respond(allow ? .allow : .deny, spokenAllow: false)
    }

    // MARK: - Input

    func handle(_ e: VoiceInputEvent) {
        guard !isEnded else { return }
        switch e {
        case .level(let db, _):
            guard !muted, !interrupted else { return }
            level = db
            process(detector.level(db, at: now(), speaking: speaker.isSpeaking))
        case .words(let text, _):
            guard !muted, !interrupted, phase != .asking else { return }
            if phase == .hearing || phase == .listening || phase == .waiting || phase == .answering || phase == .tapToAnswer {
                liveText = text
            }
            process(detector.words(at: now(), speaking: speaker.isSpeaking))
        case .interrupted(let began):
            interrupted = began
            detector.reset()
            if began {
                speaker.stopSpeaking()
                if phase == .hearing { phase = agentWorking ? .waiting : .listening }
            }
        case .routeChanged:
            detector.reset()
        case .failed(let why):
            Task { await self.end(why) }
        }
    }

    private func process(_ e: TurnDetector.Event?) {
        guard let e else { return }
        switch e {
        case .speechStarted:
            lastActivity = now()
            switch phase {
            case .asking, .transcribing, .starting, .ended:
                // Its own voice cannot answer; a turn in flight finishes first.
                return
            case .speaking:
                guard settings.bargeIn else { return }
                speaker.stopSpeaking()
            default:
                break
            }
            input.beginTurn()
            liveText = ""
            if phase != .answering && phase != .tapToAnswer { phase = .hearing } else { hearingAnswer = true }
        case .turnEnded:
            Task { await self.endTurn() }
        }
    }

    /// The owner is giving an answer to a request, not a message.
    @ObservationIgnored private var hearingAnswer = false

    private func tick() {
        guard !isEnded else { return }
        let t = now()
        if let e = detector.tick(at: t) { process(e) }
        if agentWorking, phase == .waiting, t - lastTone >= timing.waitTone, t - lastActivity >= timing.waitTone {
            lastTone = t
            speaker.tone()
        }
        if !agentWorking, phase == .listening, t - lastActivity >= timing.idleLimit {
            Task { await self.end("Ended after 10 minutes with no speech.") }
        }
    }

    /// The turn ended (silence or a tap): its text, then a command, an answer, or a message.
    func endTurn() async {
        let answering = phase == .answering || phase == .tapToAnswer || hearingAnswer
        guard phase == .hearing || answering else { return }
        hearingAnswer = false
        let resume = phase
        phase = .transcribing
        detector.reset()
        let (clip, live) = await input.endTurn()
        guard clip.duration >= 0.25 || !live.isEmpty else {
            phase = answering ? resume : (agentWorking ? .waiting : .listening)
            return
        }
        guard let r = await engines.transcribe(clip, live: live) else {
            notice = "No engine wrote text: \(engines.lastFallback ?? "")"
            phase = answering ? resume : (agentWorking ? .waiting : .listening)
            return
        }
        liveText = r.text
        lastActivity = now()
        guard !r.text.isEmpty else {
            phase = answering ? resume : (agentWorking ? .waiting : .listening)
            return
        }
        if answering {
            phase = resume
            await heardAnswer(r.text)
        } else {
            await utterance(r.text)
        }
    }

    private func utterance(_ text: String) async {
        if settings.spokenCommands, let cmd = VoiceGrammar.command(text) {
            switch cmd {
            case .stop:
                phase = agentWorking ? .waiting : .listening
                await chat.interrupt()
            case .repeatReply:
                await speak(lastSpoken ?? "Nothing to repeat yet.")
            case .allow, .deny:
                if request != nil {
                    await respond(cmd == .allow ? .allow : .deny, spokenAllow: true)
                } else {
                    await speak("Nothing is waiting for an answer.")
                }
            case .endVoice:
                await end(nil)
            }
            return
        }
        if settings.confirmBeforeSend {
            phase = .confirming(text)
            return
        }
        await deliver(text)
    }

    /// "send" while confirming sends the pending text; other words replace it.
    public func confirmSend() async {
        if case .confirming(let text) = phase { await deliver(text) }
    }

    private func deliver(_ text: String) async {
        let gate = chat.gate
        switch gate {
        case .blocked:
            await speak("Answer the request first.")
        case .noAgent:
            await speak(chat.context.sessionExited ? "The session has exited." : "No agent runs in this session.")
        case .offline:
            await speak("Lost \(chat.context.hostLabel).")
        case .now, .afterTurn:
            chat.send(SpokenText.prompt(text, askSummary: settings.askSummary), origin: .dictated)
            lastActivity = now()
            lastTone = now()
            if gate == .afterTurn {
                notice = "Queued"
                await speak("Queued.")
            } else {
                notice = nil
            }
            if phase != .speaking { phase = .waiting }
        }
    }

    // MARK: - The session

    private func observeChat() {
        withObservationTracking {
            _ = chat.context
            _ = chat.messages
        } onChange: { [weak self] in
            Task { @MainActor in
                guard let self, !self.isEnded else { return }
                self.chatChanged()
                self.observeChat()
            }
        }
    }

    func chatChanged() {
        guard !isEnded, phase != .starting else { return }
        let c = chat.context
        guard c.online else {
            notice = "Lost \(c.hostLabel)"
            return
        }
        if notice?.hasPrefix("Lost ") == true { notice = nil }
        let status = c.agentStatus
        let old = lastStatus
        lastStatus = status
        if status == .working {
            toolLine = Self.toolLine(chat.messages)
            if phase == .listening { phase = .waiting }
        }
        // A request answered elsewhere (another client, the TUI).
        if request != nil, !(status == .blocked && c.agent?.request?.id == askedRequest) {
            request = nil
            askedRequest = nil
            if phase == .answering || phase == .tapToAnswer || phase == .asking {
                speaker.stopSpeaking()
                phase = agentWorking ? .waiting : .listening
            }
        }
        if status == .blocked, let a = c.agent, a.kind == .permission, let id = a.request?.id, id != askedRequest {
            Task { await self.ask(a) }
            return
        }
        if status == .blocked, let a = c.agent, a.kind == .question, old != .blocked {
            Task { await self.speak("The agent asks a question. Answer it on screen.") }
            return
        }
        guard old == .working else {
            if phase == .waiting && status != .working { phase = .listening }
            return
        }
        switch status {
        case .done, .idle:
            Task { await self.finishTurn(stopped: false) }
        case .error:
            Task { await self.finishTurn(stopped: true) }
        default:
            break
        }
    }

    /// The turn ended: wait briefly for its reply in the chat, then read it.
    private func finishTurn(stopped: Bool) async {
        let deadline = ContinuousClock.now + timing.replyWait
        while SpokenText.finalMessage(chat.messages)?.id == lastSpokenMessage, ContinuousClock.now < deadline, !isEnded {
            try? await Task.sleep(for: .milliseconds(50))
        }
        guard !isEnded else { return }
        let m = SpokenText.finalMessage(chat.messages)
        let fresh = m != nil && m?.id != lastSpokenMessage
        lastSpokenMessage = m?.id ?? lastSpokenMessage
        toolLine = nil
        // The owner is talking: the reply stays in the chat.
        if phase == .hearing || phase == .transcribing || phase == .asking || phase == .answering { return }
        let text = fresh ? SpokenText.reply(chat.messages, stopped: stopped) : (stopped ? "The turn stopped." : "Done.")
        await speak(text)
    }

    static func toolLine(_ ms: [ChatMessage]) -> String? {
        guard let t = ms.last(where: { $0.kind == .tool && $0.parent == nil })?.tool else { return nil }
        return [t.name, t.target].compactMap { $0 }.joined(separator: " ")
    }

    // MARK: - Speech

    private func speak(_ text: String) async {
        guard !isEnded else { return }
        lastSpoken = text
        let wasAsking = phase == .asking
        if !wasAsking { phase = .speaking }
        _ = await speaker.speak(text)
        lastActivity = now()
        guard !isEnded, phase == .speaking else { return }
        phase = agentWorking ? .waiting : .listening
    }

    // MARK: - Approvals

    private func ask(_ a: AgentState) async {
        guard let r = a.request, let id = r.id else { return }
        askedRequest = id
        request = r
        requestMayAllow = VoiceGrammar.voiceMayAllow(r)
        answerAttempts = 0
        await askAloud(VoiceGrammar.readBack(r, mayAllow: requestMayAllow))
    }

    /// Speaks while not listening, so its own voice cannot answer; then listens.
    private func askAloud(_ text: String) async {
        speaker.stopSpeaking()
        phase = .asking
        input.setMuted(true)
        detector.reset()
        lastSpoken = text
        _ = await speaker.speak(text)
        if !muted { input.setMuted(false) }
        guard !isEnded, phase == .asking, request != nil else { return }
        phase = .answering
        lastActivity = now()
    }

    private func heardAnswer(_ text: String) async {
        let ans = VoiceGrammar.answer(text)
        guard let ans else {
            answerAttempts += 1
            if answerAttempts == 1 {
                await askAloud(requestMayAllow ? "Allow or deny?" : "Say deny, or tap.")
            } else {
                phase = .asking
                lastSpoken = "Tap Allow or Deny."
                _ = await speaker.speak("Tap Allow or Deny.")
                if phase == .asking, request != nil { phase = .tapToAnswer }
            }
            return
        }
        await respond(ans == .allow ? .allow : .deny, spokenAllow: true)
    }

    private func respond(_ choice: ApprovalChoice, spokenAllow: Bool) async {
        guard let r = request, let id = r.id, let pane = chat.context.pane else { return }
        if let e = r.expiresAt.flatMap(BoardRows.parseStamp), e <= Date() {
            request = nil
            await speak("That request expired. Answer it in the terminal.")
            return
        }
        if choice == .allow, spokenAllow, !requestMayAllow {
            phase = .asking
            _ = await speaker.speak("This one can be allowed only on screen.")
            if request != nil { phase = .tapToAnswer }
            return
        }
        do {
            try await answer(ApprovalAnswerParams(choice: choice, pane: pane, request: id, via: .voice))
            request = nil
            await speak(choice == .allow ? "Allowed." : "Denied.")
        } catch let e as RPCError where e.kind == .conflict {
            request = nil
            await speak("That request was already answered or expired.")
        } catch {
            await speak("Could not reach \(chat.context.hostLabel).")
            if request != nil { phase = .tapToAnswer }
        }
    }
}
