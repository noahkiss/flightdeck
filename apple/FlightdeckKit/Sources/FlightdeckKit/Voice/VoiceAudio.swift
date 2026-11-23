import AVFoundation
import Foundation
import Speech
import Synchronization

/// The real microphone and speaker for voice mode and dictation: one
/// `AVAudioEngine` with voice processing (echo cancellation), so the app's
/// own speech, played through the same engine, does not reach the
/// recognizer or start a barge-in. Apple's SpeechAnalyzer gives the live
/// words.
@MainActor
public final class VoiceAudio: VoiceInput, VoiceSpeaker {
    public let events: AsyncStream<VoiceInputEvent>
    private let sink: AsyncStream<VoiceInputEvent>.Continuation
    private let engine = AVAudioEngine()
    private let player = AVAudioPlayerNode()
    private let synthesizer = AVSpeechSynthesizer()
    private let tapState: TapState
    private var live: LiveWords?
    private var observers: [NSObjectProtocol] = []
    private var running = false
    private var speakDone: CheckedContinuation<Bool, Never>?
    private var speakID = 0
    /// Resamples the current utterance's buffers to `playFormat`.
    private var speakConverter: AVAudioConverter?
    /// Open microphones across windows; the last to stop ends the audio session.
    static var sessionUsers = 0
    /// The format the player renders in: the mixer's, mono.
    private var playFormat: AVAudioFormat?
    /// Whether the speaker side is used (voice mode) or only the microphone (dictation).
    private let speaks: Bool
    /// The owner's voice from Settings; nil picks the best installed one.
    var voiceID: () -> String? = { nil }
    private let log = AppLog(category: "voice")

    public init(speaks: Bool) {
        self.speaks = speaks
        (events, sink) = AsyncStream.makeStream(of: VoiceInputEvent.self)
        tapState = TapState(sink: sink)
    }

    public var hasLiveWords: Bool { true }

    /// Asks for the microphone. False: the owner said no.
    public static func requestPermission() async -> Bool {
        await AVAudioApplication.requestRecordPermission()
    }

    public static var permissionDenied: Bool {
        AVAudioApplication.shared.recordPermission == .denied
    }

    // MARK: - Input

    public func start() async throws {
        guard !running else { return }
        guard await Self.requestPermission() else {
            throw SpeechEngineError.unavailable("microphone access is off", sticky: true)
        }
        #if os(iOS)
        let session = AVAudioSession.sharedInstance()
        // Another window's microphone already set the session: keep its mode.
        if Self.sessionUsers == 0 {
            let c = Self.sessionConfig(speaks: speaks)
            try session.setCategory(c.category, mode: c.mode, options: c.options)
        }
        try session.setActive(true)
        observe(session)
        #endif
        if speaks {
            // Echo cancellation for the app's own voice; it needs the output in the same engine.
            try? engine.inputNode.setVoiceProcessingEnabled(true)
            engine.attach(player)
            let out = engine.mainMixerNode.outputFormat(forBus: 0)
            let mono = AVAudioFormat(standardFormatWithSampleRate: out.sampleRate > 0 ? out.sampleRate : 48_000, channels: 1)
            playFormat = mono
            engine.connect(player, to: engine.mainMixerNode, format: mono)
        }
        let input = engine.inputNode
        let format = input.outputFormat(forBus: 0)
        guard format.sampleRate > 0, format.channelCount > 0 else {
            throw SpeechEngineError.unavailable("no microphone input", sticky: true)
        }
        tapState.reset(live: nil)
        input.installTap(onBus: 0, bufferSize: 2048, format: format, block: Self.tapBlock(tapState, input: format))
        engine.prepare()
        try engine.start()
        if speaks {
            player.play()
            useSpeaker()
        }
        running = true
        Self.sessionUsers += 1
        // The microphone runs now; live words follow, after their asset downloads.
        let words = LiveWords(onText: { [weak self] finished, volatile in
            Task { @MainActor in self?.tapState.liveChanged(finished: finished, volatile: volatile) }
        }, onStatus: { [weak self] status in
            Task { @MainActor in self?.sink.yield(.liveWords(status)) }
        })
        live = words
        Task { [weak self] in
            await words.start(natural: format)
            guard let self, self.running, self.live === words else { return }
            self.tapState.attach(words)
        }
        observers.append(NotificationCenter.default.addObserver(forName: .AVAudioEngineConfigurationChange, object: engine, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.restart() }
        })
    }

    public func stop() {
        guard running else { return }
        running = false
        stopSpeaking()
        engine.inputNode.removeTap(onBus: 0)
        engine.stop()
        Task { [live] in await live?.stop() }
        live = nil
        for o in observers { NotificationCenter.default.removeObserver(o) }
        observers = []
        Self.sessionUsers = max(0, Self.sessionUsers - 1)
        #if os(iOS)
        // Another window's voice mode or dictation may still use the session.
        if Self.sessionUsers == 0 {
            try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        }
        #endif
        sink.finish()
    }

    public func setMuted(_ muted: Bool) {
        tapState.setMuted(muted)
    }

    public func beginTurn() {
        tapState.beginTurn()
    }

    public func endTurn() async -> (clip: SpeechClip, live: String) {
        tapState.endTurn()
    }

    /// The microphone tap. AVAudioEngine calls it on its audio thread, so it is
    /// made here, outside the main actor: a closure written inside a main-actor
    /// method would be main-actor isolated and trap on that thread.
    /// The block owns its converters: only the audio thread uses them.
    private nonisolated static func tapBlock(_ state: TapState, input: AVAudioFormat) -> AVAudioNodeTapBlock {
        let converters = TapConverters(input: input)
        return { buffer, _ in state.take(buffer, with: converters) }
    }

    /// The synthesizer's buffer callback, which runs off the main actor (see
    /// `tapBlock`). AVFoundation does not say its buffer outlives the callback,
    /// so the callback hands over a copy it made itself.
    private nonisolated static func speechBuffers(_ deliver: @escaping @Sendable (AudioBufferHandoff) -> Void) -> AVSpeechSynthesizer.BufferCallback {
        { buffer in
            guard let pcm = buffer as? AVAudioPCMBuffer, let own = copy(pcm) else { return }
            deliver(AudioBufferHandoff(own))
        }
    }

    /// A new buffer with the same format, length and samples. An empty buffer
    /// (the synthesizer's end) copies as an empty one.
    nonisolated static func copy(_ buffer: AVAudioPCMBuffer) -> AVAudioPCMBuffer? {
        guard let out = AVAudioPCMBuffer(pcmFormat: buffer.format, frameCapacity: max(buffer.frameLength, 1)) else { return nil }
        out.frameLength = buffer.frameLength
        let from = UnsafeMutableAudioBufferListPointer(UnsafeMutablePointer(mutating: buffer.audioBufferList))
        let to = UnsafeMutableAudioBufferListPointer(out.mutableAudioBufferList)
        for (s, d) in zip(from, to) {
            guard let src = s.mData, let dst = d.mData else { continue }
            memcpy(dst, src, Int(min(s.mDataByteSize, d.mDataByteSize)))
        }
        return out
    }

    /// A route change or a reset of the audio hardware: start again in place.
    private func restart() {
        guard running else { return }
        engine.stop()
        engine.inputNode.removeTap(onBus: 0)
        let format = engine.inputNode.outputFormat(forBus: 0)
        guard format.sampleRate > 0 else { return }
        tapState.reset(live: live)
        engine.inputNode.installTap(onBus: 0, bufferSize: 2048, format: format, block: Self.tapBlock(tapState, input: format))
        do {
            try engine.start()
            if speaks {
                player.play()
                useSpeaker()
            }
            sink.yield(.routeChanged)
        } catch {
            sink.yield(.failed("the audio input did not restart: \(error.localizedDescription)"))
        }
    }

    #if os(iOS)
    /// The audio session voice mode and dictation set (app.md 13.7, "Audio
    /// session"). Both use `playAndRecord`, which plays with the silent switch
    /// on, as a navigation or podcast app does: the owner turned voice on.
    /// Voice mode keeps the microphone open while it speaks (barge-in, echo
    /// cancellation), so it never switches to `playback`, which has no input.
    /// Both duck other audio and route to the speaker or a Bluetooth headset.
    /// Notification sounds are not this session's: the switch still governs them.
    static func sessionConfig(speaks: Bool) -> (category: AVAudioSession.Category, mode: AVAudioSession.Mode, options: AVAudioSession.CategoryOptions) {
        if speaks {
            return (.playAndRecord, .voiceChat, [.defaultToSpeaker, .allowBluetoothHFP, .allowBluetoothA2DP, .duckOthers])
        }
        return (.playAndRecord, .default, [.defaultToSpeaker, .allowBluetoothHFP, .duckOthers])
    }

    /// Whether speech would leave by the earpiece: the phone's receiver is the
    /// only output. Voice processing can move the route there even with
    /// `defaultToSpeaker`, and away from the ear that sounds silent.
    static func needsSpeaker(outputs: [AVAudioSession.Port]) -> Bool {
        outputs == [.builtInReceiver]
    }

    private func observe(_ session: AVAudioSession) {
        observers.append(NotificationCenter.default.addObserver(forName: AVAudioSession.interruptionNotification, object: session, queue: .main) { [weak self] n in
            let raw = n.userInfo?[AVAudioSessionInterruptionTypeKey] as? UInt
            let began = raw.flatMap(AVAudioSession.InterruptionType.init) == .began
            Task { @MainActor in
                guard let self else { return }
                if began {
                    self.stopSpeaking()
                } else {
                    try? AVAudioSession.sharedInstance().setActive(true)
                    self.restart()
                }
                self.sink.yield(.interrupted(began))
            }
        })
    }
    #endif

    // MARK: - Speaker

    /// Voice mode speaks from the loudspeaker when no headset is connected.
    /// A headset, AirPods or a car keep their route.
    private func useSpeaker() {
        #if os(iOS)
        let session = AVAudioSession.sharedInstance()
        let outputs = session.currentRoute.outputs.map(\.portType)
        guard Self.needsSpeaker(outputs: outputs) else { return }
        do {
            try session.overrideOutputAudioPort(.speaker)
        } catch {
            log.error("speaker override failed: \(kind: error) \(error.localizedDescription)")
        }
        #endif
    }

    public var isSpeaking: Bool { speakDone != nil }

    public func speak(_ text: String) async -> Bool {
        stopSpeaking()
        guard speaks, running, let fmt = playFormat else { return false }
        #if os(iOS)
        // Speech must never play under a category the silent switch mutes.
        let session = AVAudioSession.sharedInstance()
        if session.category != .playAndRecord {
            let c = Self.sessionConfig(speaks: true)
            try? session.setCategory(c.category, mode: c.mode, options: c.options)
            try? session.setActive(true)
        }
        #endif
        useSpeaker()
        speakID += 1
        let id = speakID
        let utterance = AVSpeechUtterance(string: text)
        utterance.voice = SpeechVoices.system(chosen: voiceID())
        // A watchdog: if the synthesizer never ends (no voice, an engine
        // restart), give up after the expected time plus a margin.
        let words = text.split(separator: " ").count
        let limit = Duration.seconds(Double(words) / 2.0 + 5)
        Task { [weak self] in
            try? await Task.sleep(for: limit)
            guard let self, self.speakID == id, self.speakDone != nil else { return }
            self.stopSpeaking()
        }
        return await withCheckedContinuation { c in
            speakDone = c
            speakConverter = nil
            synthesizer.write(utterance, toBufferCallback: Self.speechBuffers { [weak self] chunk in
                Task { @MainActor in await self?.playSpeech(chunk.buffer, id: id, format: fmt) }
            })
        }
    }

    /// One buffer from the synthesizer; an empty one is the end.
    private func playSpeech(_ pcm: AVAudioPCMBuffer, id: Int, format fmt: AVAudioFormat) async {
        guard speakID == id, speakDone != nil else { return }
        if pcm.frameLength == 0 {
            // The end: finish once the queued audio has played.
            await player.scheduleBuffer(AVAudioPCMBuffer(pcmFormat: fmt, frameCapacity: 1)!, completionCallbackType: .dataPlayedBack)
            finishSpeaking(id, finished: true)
            return
        }
        if speakConverter == nil || speakConverter?.inputFormat != pcm.format {
            speakConverter = AVAudioConverter(from: pcm.format, to: fmt)
        }
        if let out = speakConverter.flatMap({ Self.convert(pcm, with: $0, to: fmt) }) {
            schedule(out)
        }
    }

    /// Queues one buffer of speech. Synchronous on purpose: the player keeps
    /// the order, and nothing waits for a buffer but the last.
    private func schedule(_ buffer: AVAudioPCMBuffer) {
        player.scheduleBuffer(buffer)
    }

    public func stopSpeaking() {
        guard speakDone != nil else { return }
        speakID += 1
        synthesizer.stopSpeaking(at: .immediate)
        player.stop()
        if running { player.play() }
        let c = speakDone
        speakDone = nil
        c?.resume(returning: false)
    }

    private func finishSpeaking(_ id: Int, finished: Bool) {
        guard id == speakID, let c = speakDone else { return }
        speakDone = nil
        c.resume(returning: finished)
    }

    public func tone() {
        play(cue: [(660, 0.12)])
    }

    public func chime() {
        play(cue: [(880, 0.1), (1175, 0.14)])
    }

    /// Short sine tones, quiet, through the player.
    private func play(cue: [(Double, Double)]) {
        guard speaks, running, let fmt = playFormat else { return }
        for (hz, seconds) in cue {
            let n = AVAudioFrameCount(fmt.sampleRate * seconds)
            guard let b = AVAudioPCMBuffer(pcmFormat: fmt, frameCapacity: n) else { continue }
            b.frameLength = n
            let ch = b.floatChannelData![0]
            for i in 0..<Int(n) {
                let t = Double(i) / fmt.sampleRate
                let fade = min(1, min(t, seconds - t) / 0.02)
                ch[i] = Float(0.15 * fade * sin(2 * .pi * hz * t))
            }
            player.scheduleBuffer(b)
        }
    }

    nonisolated static func convert(_ buffer: AVAudioPCMBuffer, with converter: AVAudioConverter, to format: AVAudioFormat) -> AVAudioPCMBuffer? {
        let ratio = format.sampleRate / buffer.format.sampleRate
        let capacity = AVAudioFrameCount(Double(buffer.frameLength) * ratio) + 1024
        guard let out = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: capacity) else { return nil }
        var given = false
        var error: NSError?
        converter.convert(to: out, error: &error) { _, status in
            if given {
                status.pointee = .noDataNow
                return nil
            }
            given = true
            status.pointee = .haveData
            return buffer
        }
        return error == nil ? out : nil
    }
}

/// One audio buffer handed from one thread to another.
///
/// AVAudioPCMBuffer is not Sendable, and AVFAudio gives no Sendable buffer
/// before iOS 27, so this one escape is needed. It is safe by construction:
/// the sender allocates each buffer itself (a converter's output, or its own
/// copy of a synthesizer buffer), hands it over at once and keeps no
/// reference, so the receiver is its only user.
struct AudioBufferHandoff: @unchecked Sendable {
    let buffer: AVAudioPCMBuffer
    init(_ buffer: AVAudioPCMBuffer) { self.buffer = buffer }
}

/// The tap's resamplers: to the clip format, and to the live recognizer's.
/// Only one tap block holds it, and AVAudioEngine calls that block on its
/// audio thread one buffer at a time, so it needs no lock and never crosses
/// threads. A route change installs a new tap with new converters.
final class TapConverters {
    let input: AVAudioFormat
    private let toClip: AVAudioConverter?
    private var toLive: (format: AVAudioFormat, converter: AVAudioConverter)?

    init(input: AVAudioFormat) {
        self.input = input
        toClip = AVAudioConverter(from: input, to: TapState.target)
    }

    /// The buffer as 16 kHz mono.
    func clip(_ buffer: AVAudioPCMBuffer) -> AVAudioPCMBuffer? {
        toClip.flatMap { VoiceAudio.convert(buffer, with: $0, to: TapState.target) }
    }

    /// The buffer in the live recognizer's format.
    func live(_ buffer: AVAudioPCMBuffer, to format: AVAudioFormat) -> AVAudioPCMBuffer? {
        if toLive?.format != format {
            toLive = AVAudioConverter(from: input, to: format).map { (format, $0) }
        }
        return toLive.flatMap { VoiceAudio.convert(buffer, with: $0.converter, to: format) }
    }
}

/// The audio thread's side: resamples to 16 kHz mono, reports the level,
/// keeps the turn's samples (with a short lead-in), and feeds the live recognizer.
/// The main actor starts and ends turns; the audio thread adds samples.
final class TapState: Sendable {
    static let target = AVAudioFormat(standardFormatWithSampleRate: Double(SpeechClip.sampleRate), channels: 1)!
    /// Lead-in kept before a turn starts, so its first word is not cut.
    static let leadIn = SpeechClip.sampleRate / 2
    static let maxTurn = Int(SpeechLimits.maxClipSeconds) * SpeechClip.sampleRate

    private struct State {
        weak var live: LiveWords?
        var muted = false
        var inTurn = false
        var full = false
        var samples: [Float] = []
        var text = LiveText()
    }

    private let state = Mutex(State())
    private let sink: AsyncStream<VoiceInputEvent>.Continuation
    private let clock = ContinuousClock()
    private let start = ContinuousClock.now

    init(sink: AsyncStream<VoiceInputEvent>.Continuation) {
        self.sink = sink
    }

    /// A new tap: the live recognizer it feeds, if any yet.
    func reset(live: LiveWords?) {
        state.withLock { $0.live = live }
    }

    func attach(_ live: LiveWords) {
        state.withLock { $0.live = live }
    }

    func setMuted(_ m: Bool) {
        state.withLock { s in
            s.muted = m
            if m { s.samples.removeAll(); s.inTurn = false; s.full = false }
        }
    }

    func beginTurn() {
        let t = now()
        state.withLock { s in
            s.inTurn = true
            s.full = false
            s.text.begin(at: t)
        }
    }

    func endTurn() -> (clip: SpeechClip, live: String) {
        state.withLock { s in
            let clip = SpeechClip(samples: s.samples)
            let t = s.text.turn
            s.samples.removeAll(keepingCapacity: true)
            s.inTurn = false
            s.full = false
            s.text.end()
            return (clip, t)
        }
    }

    func liveChanged(finished: String, volatile: String) {
        let t = now()
        let event: VoiceInputEvent? = state.withLock { s in
            s.text.update(finished: finished, volatile: volatile, at: t)
            let turn = s.text.turn
            return s.muted || turn.isEmpty ? nil : .words(turn, at: t)
        }
        if let event { sink.yield(event) }
    }

    /// One microphone buffer, on the audio thread.
    func take(_ buffer: AVAudioPCMBuffer, with converters: TapConverters) {
        let (words, isMuted) = state.withLock { ($0.live, $0.muted) }
        if isMuted { return }
        words?.feed(buffer, with: converters)
        guard let out = converters.clip(buffer), let data = out.floatChannelData else { return }
        let chunk = Array(UnsafeBufferPointer(start: data[0], count: Int(out.frameLength)))
        let db = chunk.withUnsafeBufferPointer { audioLevel($0) }
        let reachedFull: Bool = state.withLock { s in
            if !s.inTurn {
                s.samples.append(contentsOf: chunk)
                if s.samples.count > Self.leadIn { s.samples.removeFirst(s.samples.count - Self.leadIn) }
                return false
            }
            guard !s.full else { return false }
            let room = Self.maxTurn - s.samples.count
            s.samples.append(contentsOf: chunk.prefix(max(0, room)))
            if s.samples.count >= Self.maxTurn {
                // The hab takes at most 120 s: the turn ends here, whole.
                s.full = true
                return true
            }
            return false
        }
        sink.yield(.level(db, at: now()))
        if reachedFull { sink.yield(.turnFull) }
    }

    private func now() -> Double {
        let d = clock.now - start
        return Double(d.components.seconds) + Double(d.components.attoseconds) / 1e18
    }
}
/// The live recognizer's text, split by turn. Finished results only grow,
/// so a turn is the finished text since it began plus the result in
/// progress; a revision of the result in progress never brings back earlier speech.
struct LiveText: Sendable {
    private(set) var finished = ""
    private(set) var volatile = ""
    /// Where the current (or next) turn's finished text starts.
    private var base = 0
    /// The finished length before the last result that finished, and when.
    private var lastFinal: (before: Int, at: Double)?
    /// A result that finished this close before a turn began belongs to it:
    /// its words started the turn.
    static let carry = 0.6
    /// The ended turn still had a phrase in progress: it belongs to that turn.
    /// Its later revisions and its final result are skipped.
    private var tailOfEnded = false

    mutating func update(finished f: String, volatile v: String, at t: Double) {
        if f.count > finished.count {
            if tailOfEnded {
                // The ended turn's phrase finished: move past it, with no carry.
                tailOfEnded = false
                base = f.count
                lastFinal = nil
            } else {
                lastFinal = (finished.count, t)
            }
        }
        finished = f
        volatile = v
    }

    mutating func begin(at t: Double) {
        base = finished.count
        if let l = lastFinal, t - l.at <= Self.carry, l.before >= 0 { base = min(base, l.before) }
    }

    mutating func end() {
        base = finished.count
        lastFinal = nil
        tailOfEnded = !volatile.isEmpty
    }

    var turn: String {
        let start = finished.index(finished.startIndex, offsetBy: min(base, finished.count))
        return (String(finished[start...]) + (tailOfEnded ? "" : volatile))
            .replacingOccurrences(of: "\\s+", with: " ", options: .regularExpression)
            .trimmingCharacters(in: .whitespacesAndNewlines)
    }
}

/// Apple's live recognizer over the microphone (SpeechAnalyzer).
/// It reports the whole text heard so far: finished results plus the one in progress.
final class LiveWords: Sendable {
    private struct State {
        var feed: AsyncStream<AudioBufferHandoff>.Continuation?
        var format: AVAudioFormat?
        var task: Task<Void, Never>?
        var finished = ""
        var volatile = ""
        var stopped = false
    }

    private let state = Mutex(State())
    private let onText: @Sendable (_ finished: String, _ volatile: String) -> Void
    private let onStatus: @Sendable (String?) -> Void

    init(onText: @escaping @Sendable (_ finished: String, _ volatile: String) -> Void, onStatus: @escaping @Sendable (String?) -> Void) {
        self.onText = onText
        self.onStatus = onStatus
    }

    func start(natural: AVAudioFormat) async {
        guard SpeechTranscriber.isAvailable,
              let locale = await SpeechTranscriber.supportedLocale(equivalentTo: Locale(identifier: "en-US")) else {
            onStatus("Live words are off: no English speech model on this device")
            return
        }
        let transcriber = SpeechTranscriber(locale: locale, transcriptionOptions: [], reportingOptions: [.volatileResults], attributeOptions: [])
        do {
            if let req = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
                onStatus("Downloading speech for live words")
                try await req.downloadAndInstall()
            }
        } catch {
            onStatus("Live words are off: \(error.localizedDescription)")
            return
        }
        guard let fmt = await SpeechAnalyzer.bestAvailableAudioFormat(compatibleWith: [transcriber], considering: natural) else {
            onStatus("Live words are off: no audio format fits")
            return
        }
        onStatus(nil)
        let (stream, cont) = AsyncStream.makeStream(of: AnalyzerInput.self)
        let analyzer = SpeechAnalyzer(modules: [transcriber])
        let (buffers, feed) = AsyncStream.makeStream(of: AudioBufferHandoff.self)
        let pump = Task {
            for await b in buffers { cont.yield(AnalyzerInput(buffer: b.buffer)) }
            cont.finish()
        }
        let task = Task { [weak self] in
            do {
                try await analyzer.start(inputSequence: stream)
                for try await r in transcriber.results {
                    guard let self else { break }
                    let t = String(r.text.characters)
                    let (f, v): (String, String) = self.state.withLock { s in
                        if r.isFinal {
                            s.finished += t
                            s.volatile = ""
                        } else {
                            s.volatile = t
                        }
                        return (s.finished, s.volatile)
                    }
                    self.onText(f, v)
                }
            } catch {
                self?.onStatus("Live words stopped: \(error.localizedDescription)")
            }
            pump.cancel()
            await analyzer.cancelAndFinishNow()
        }
        // `stop` may have run during the download: then nothing is left running.
        let stoppedMeanwhile: Bool = state.withLock { s in
            if s.stopped { return true }
            s.format = fmt
            s.feed = feed
            s.task = task
            return false
        }
        if stoppedMeanwhile {
            feed.finish()
            task.cancel()
        }
    }

    /// One microphone buffer, on the audio thread.
    func feed(_ buffer: AVAudioPCMBuffer, with converters: TapConverters) {
        let (fmt, sink) = state.withLock { ($0.format, $0.feed) }
        guard let fmt, let sink, let out = converters.live(buffer, to: fmt) else { return }
        sink.yield(AudioBufferHandoff(out))
    }

    func stop() async {
        let (sink, task) = state.withLock { s in
            s.stopped = true
            defer { s.feed = nil }
            return (s.feed, s.task)
        }
        sink?.finish()
        task?.cancel()
    }
}
