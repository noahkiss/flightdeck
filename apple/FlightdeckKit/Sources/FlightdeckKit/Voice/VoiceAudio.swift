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
    /// The format the player renders in: the mixer's, mono.
    private var playFormat: AVAudioFormat?
    /// Whether the speaker side is used (voice mode) or only the microphone (dictation).
    private let speaks: Bool
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
        try session.setCategory(.playAndRecord, mode: speaks ? .voiceChat : .default,
                                options: [.defaultToSpeaker, .allowBluetoothHFP, .allowBluetoothA2DP])
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
        if LiveWords.isSupported {
            let words = LiveWords { [weak self] text in
                Task { @MainActor in self?.tapState.liveChanged(text) }
            }
            live = words
            await words.start(natural: format)
        }
        tapState.reset(input: format, live: live)
        input.installTap(onBus: 0, bufferSize: 2048, format: format) { [tapState] buffer, _ in
            tapState.take(buffer)
        }
        engine.prepare()
        try engine.start()
        if speaks { player.play() }
        running = true
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
        #if os(iOS)
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
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
        speakID += 1
        let id = speakID
        let utterance = AVSpeechUtterance(string: text)
        utterance.voice = Self.voice()
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

    /// A premium English voice when one is installed, else an enhanced one.
    static func voice() -> AVSpeechSynthesisVoice? {
        let en = AVSpeechSynthesisVoice.speechVoices().filter { $0.language.hasPrefix("en") }
        let preferred = Locale.current.language.languageCode?.identifier == "en" ? AVSpeechSynthesisVoice.currentLanguageCode() : "en-US"
        for q in [AVSpeechSynthesisVoiceQuality.premium, .enhanced] {
            if let v = en.first(where: { $0.quality == q && $0.language == preferred }) ?? en.first(where: { $0.quality == q }) {
                return v
            }
        }
        return AVSpeechSynthesisVoice(language: preferred)
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
    private var samples: [Float] = []
    private var liveText = ""
    private var liveBase = ""
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

    func setMuted(_ m: Bool) {
        lock.withLock {
            muted = m
            if m { samples.removeAll(); inTurn = false }
        }
    }

    func beginTurn() {
        lock.withLock {
            inTurn = true
            liveBase = liveText
        }
    }

    func endTurn() -> (clip: SpeechClip, live: String) {
        lock.withLock {
            let clip = SpeechClip(samples: samples)
            let text = Self.turnText(all: liveText, base: liveBase)
            samples.removeAll(keepingCapacity: true)
            inTurn = false
            liveBase = liveText
            return (clip, text)
        }
    }

    /// The live text heard since the turn began: what was there at its start is cut off.
    static func turnText(all: String, base: String) -> String {
        if !base.isEmpty, all.hasPrefix(base) {
            return String(all.dropFirst(base.count)).trimmingCharacters(in: .whitespacesAndNewlines)
        }
        return all.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    func liveChanged(_ text: String) {
        let (event, report): (VoiceInputEvent?, Bool) = lock.withLock {
            liveText = text
            let t = Self.turnText(all: text, base: liveBase)
            return (muted || t.isEmpty ? nil : .words(t, at: now()), true)
        }
        if report, let event { sink.yield(event) }
    }

    func take(_ buffer: AVAudioPCMBuffer) {
        let (conv, words, isMuted) = lock.withLock { (converter, live, muted) }
        if isMuted { return }
        words?.feed(buffer)
        guard let conv, let out = VoiceAudio.convert(buffer, with: conv, to: target), let data = out.floatChannelData else { return }
        let n = Int(out.frameLength)
        let chunk = UnsafeBufferPointer(start: data[0], count: n)
        let db = audioLevel(chunk)
        lock.withLock {
            samples.append(contentsOf: chunk)
            if !inTurn, samples.count > Self.leadIn {
                samples.removeFirst(samples.count - Self.leadIn)
            } else if samples.count > Self.maxTurn {
                samples.removeFirst(samples.count - Self.maxTurn)
            }
        }
        sink.yield(.level(db, at: now()))
    }

    private func now() -> Double {
        let d = clock.now - start
        return Double(d.components.seconds) + Double(d.components.attoseconds) / 1e18
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
    private let onText: @Sendable (String) -> Void
    private var feedSink: AsyncStream<AnyObject>.Continuation?
    private var converter: AVAudioConverter?
    private var format: AVAudioFormat?
    private var task: Task<Void, Never>?
    private var finished = ""
    private var volatile = ""

    init(onText: @escaping @Sendable (String) -> Void) {
        self.onText = onText
    }

    func start(natural: AVAudioFormat) async {
        guard #available(iOS 26, macOS 26, *) else { return }
        guard SpeechTranscriber.isAvailable,
              let locale = await SpeechTranscriber.supportedLocale(equivalentTo: Locale(identifier: "en-US")) else { return }
        let transcriber = SpeechTranscriber(locale: locale, transcriptionOptions: [], reportingOptions: [.volatileResults], attributeOptions: [])
        do {
            if let req = try await AssetInventory.assetInstallationRequest(supporting: [transcriber]) {
                try await req.downloadAndInstall()
            }
        } catch {
            return
        }
        guard let fmt = await SpeechAnalyzer.bestAvailableAudioFormat(compatibleWith: [transcriber], considering: natural) else { return }
        let (stream, cont) = AsyncStream.makeStream(of: AnalyzerInput.self)
        let analyzer = SpeechAnalyzer(modules: [transcriber])
        lock.withLock {
            format = fmt
            converter = AVAudioConverter(from: natural, to: fmt)
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
                    let text: String = self.lock.withLock {
                        if r.isFinal {
                            self.finished += t
                            self.volatile = ""
                        } else {
                            self.volatile = t
                        }
                        return self.finished + self.volatile
                    }
                    self.onText(text)
                }
            } catch {}
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
