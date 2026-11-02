import AVFoundation
import Foundation
import Speech
import os

/// The real microphone and speaker for voice mode and dictation: one
/// `AVAudioEngine` with voice processing (echo cancellation), so the app's
/// own speech, played through the same engine, does not reach the
/// recognizer or start a barge-in. Apple's SpeechAnalyzer gives the live
/// words (iOS and macOS 26 and later).
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
    /// Open microphones across windows; the last to stop ends the audio session.
    static var sessionUsers = 0
    /// The format the player renders in: the mixer's, mono.
    private var playFormat: AVAudioFormat?
    /// Whether the speaker side is used (voice mode) or only the microphone (dictation).
    private let speaks: Bool
    /// The owner's voice from Settings; nil picks the best installed one.
    var voiceID: () -> String? = { nil }
    private let log = Logger(subsystem: "com.flightdeck.app", category: "voice")

    public init(speaks: Bool) {
        self.speaks = speaks
        (events, sink) = AsyncStream.makeStream(of: VoiceInputEvent.self)
        tapState = TapState(sink: sink)
    }

    public var hasLiveWords: Bool { LiveWords.isSupported }

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
        tapState.reset(input: format, live: nil)
        input.installTap(onBus: 0, bufferSize: 2048, format: format) { [tapState] buffer, _ in
            tapState.take(buffer)
        }
        engine.prepare()
        try engine.start()
        if speaks { player.play() }
        running = true
        Self.sessionUsers += 1
        // The microphone runs now; live words follow, after their asset downloads.
        if LiveWords.isSupported {
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
        } else {
            sink.yield(.liveWords("Live words need iOS 26 or macOS 26"))
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

    /// A route change or a reset of the audio hardware: start again in place.
    private func restart() {
        guard running else { return }
        engine.stop()
        engine.inputNode.removeTap(onBus: 0)
        let format = engine.inputNode.outputFormat(forBus: 0)
        guard format.sampleRate > 0 else { return }
        live?.reset(natural: format)
        tapState.reset(input: format, live: live)
        engine.inputNode.installTap(onBus: 0, bufferSize: 2048, format: format) { [tapState] buffer, _ in
            tapState.take(buffer)
        }
        do {
            try engine.start()
            if speaks { player.play() }
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
            var converter: AVAudioConverter?
            synthesizer.write(utterance) { [weak self] buffer in
                guard let pcm = buffer as? AVAudioPCMBuffer else { return }
                Task { @MainActor in
                    guard let self, self.speakID == id, self.speakDone != nil else { return }
                    if pcm.frameLength == 0 {
                        // The end: finish once the queued audio has played.
                        self.player.scheduleBuffer(AVAudioPCMBuffer(pcmFormat: fmt, frameCapacity: 1)!, completionCallbackType: .dataPlayedBack) { _ in
                            Task { @MainActor in self.finishSpeaking(id, finished: true) }
                        }
                        return
                    }
                    if converter == nil || converter?.inputFormat != pcm.format {
                        converter = AVAudioConverter(from: pcm.format, to: fmt)
                    }
                    if let out = converter.flatMap({ Self.convert(pcm, with: $0, to: fmt) }) {
                        self.player.scheduleBuffer(out)
                    }
                }
            }
        }
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

/// The audio thread's side: resamples to 16 kHz mono, reports the level,
/// keeps the turn's samples (with a short lead-in), and feeds the live recognizer.
final class TapState: @unchecked Sendable {
    private let lock = NSLock()
    private let sink: AsyncStream<VoiceInputEvent>.Continuation
    private var converter: AVAudioConverter?
    private var target = AVAudioFormat(standardFormatWithSampleRate: Double(SpeechClip.sampleRate), channels: 1)!
    private weak var live: LiveWords?
    private var muted = false
    private var inTurn = false
    private var full = false
    private var samples: [Float] = []
    private var text = LiveText()
    private let clock = ContinuousClock()
    private let start = ContinuousClock.now
    /// Lead-in kept before a turn starts, so its first word is not cut.
    static let leadIn = SpeechClip.sampleRate / 2
    static let maxTurn = Int(SpeechLimits.maxClipSeconds) * SpeechClip.sampleRate

    init(sink: AsyncStream<VoiceInputEvent>.Continuation) {
        self.sink = sink
    }

    func reset(input: AVAudioFormat, live: LiveWords?) {
        lock.withLock {
            converter = AVAudioConverter(from: input, to: target)
            self.live = live
        }
    }

    func attach(_ live: LiveWords) {
        lock.withLock { self.live = live }
    }

    func setMuted(_ m: Bool) {
        lock.withLock {
            muted = m
            if m { samples.removeAll(); inTurn = false; full = false }
        }
    }

    func beginTurn() {
        lock.withLock {
            inTurn = true
            full = false
            text.begin(at: now())
        }
    }

    func endTurn() -> (clip: SpeechClip, live: String) {
        lock.withLock {
            let clip = SpeechClip(samples: samples)
            let t = text.turn
            samples.removeAll(keepingCapacity: true)
            inTurn = false
            full = false
            text.end()
            return (clip, t)
        }
    }

    func liveChanged(finished: String, volatile: String) {
        let event: VoiceInputEvent? = lock.withLock {
            text.update(finished: finished, volatile: volatile, at: now())
            let t = text.turn
            return muted || t.isEmpty ? nil : .words(t, at: now())
        }
        if let event { sink.yield(event) }
    }

    func take(_ buffer: AVAudioPCMBuffer) {
        let (conv, words, isMuted) = lock.withLock { (converter, live, muted) }
        if isMuted { return }
        words?.feed(buffer)
        guard let conv, let out = VoiceAudio.convert(buffer, with: conv, to: target), let data = out.floatChannelData else { return }
        let n = Int(out.frameLength)
        let chunk = UnsafeBufferPointer(start: data[0], count: n)
        let db = audioLevel(chunk)
        let reachedFull: Bool = lock.withLock {
            if !inTurn {
                samples.append(contentsOf: chunk)
                if samples.count > Self.leadIn { samples.removeFirst(samples.count - Self.leadIn) }
                return false
            }
            guard !full else { return false }
            let room = Self.maxTurn - samples.count
            samples.append(contentsOf: chunk.prefix(max(0, room)))
            if samples.count >= Self.maxTurn {
                // The hab takes at most 120 s: the turn ends here, whole.
                full = true
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

/// Apple's live recognizer over the microphone (SpeechAnalyzer, 26 and later).
/// It reports the whole text heard so far: finished results plus the one in progress.
final class LiveWords: @unchecked Sendable {
    static var isSupported: Bool {
        if #available(iOS 26, macOS 26, *) { return true }
        return false
    }

    private let lock = NSLock()
    private let onText: @Sendable (_ finished: String, _ volatile: String) -> Void
    private let onStatus: @Sendable (String?) -> Void
    private var feedSink: AsyncStream<AnyObject>.Continuation?
    private var converter: AVAudioConverter?
    private var format: AVAudioFormat?
    private var task: Task<Void, Never>?
    private var finished = ""
    private var volatile = ""

    init(onText: @escaping @Sendable (_ finished: String, _ volatile: String) -> Void, onStatus: @escaping @Sendable (String?) -> Void) {
        self.onText = onText
        self.onStatus = onStatus
    }

    /// The microphone's format changed (a route change): rebuild the converter.
    /// During the asset download it is kept for `start`.
    func reset(natural: AVAudioFormat) {
        lock.withLock {
            latestNatural = natural
            if let f = format { converter = AVAudioConverter(from: natural, to: f) }
        }
    }

    private var latestNatural: AVAudioFormat?

    func start(natural: AVAudioFormat) async {
        guard #available(iOS 26, macOS 26, *) else { return }
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
        lock.withLock {
            format = fmt
            converter = AVAudioConverter(from: latestNatural ?? natural, to: fmt)
            feedSink = nil
        }
        // Buffers go through a type-erased sink so this class needs no availability mark.
        let (erased, erasedSink) = AsyncStream.makeStream(of: AnyObject.self)
        lock.withLock { feedSink = erasedSink }
        let pump = Task {
            for await b in erased {
                if let pcm = b as? AVAudioPCMBuffer { cont.yield(AnalyzerInput(buffer: pcm)) }
            }
            cont.finish()
        }
        task = Task { [weak self] in
            do {
                try await analyzer.start(inputSequence: stream)
                for try await r in transcriber.results {
                    guard let self else { break }
                    let t = String(r.text.characters)
                    let (f, v): (String, String) = self.lock.withLock {
                        if r.isFinal {
                            self.finished += t
                            self.volatile = ""
                        } else {
                            self.volatile = t
                        }
                        return (self.finished, self.volatile)
                    }
                    self.onText(f, v)
                }
            } catch {
                self?.onStatus("Live words stopped: \(error.localizedDescription)")
            }
            pump.cancel()
            await analyzer.cancelAndFinishNow()
        }
    }

    func feed(_ buffer: AVAudioPCMBuffer) {
        let (conv, fmt, sink) = lock.withLock { (converter, format, feedSink) }
        guard let conv, let fmt, let sink, let out = VoiceAudio.convert(buffer, with: conv, to: fmt) else { return }
        sink.yield(out)
    }

    func stop() async {
        let sink = lock.withLock { () -> AsyncStream<AnyObject>.Continuation? in
            let s = feedSink
            feedSink = nil
            return s
        }
        sink?.finish()
        task?.cancel()
    }
}
