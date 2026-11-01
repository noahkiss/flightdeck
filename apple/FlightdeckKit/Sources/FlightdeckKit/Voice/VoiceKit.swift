import Foundation
import HabAPI
import Observation
import SwiftUI

/// Dictation into the chat composer or a pane (app.md 13.4): the same
/// engines as voice mode, one turn from start to stop.
@MainActor
@Observable
public final class DictationModel {
    public enum Phase: Equatable, Sendable {
        case idle
        case listening
        case transcribing
    }

    public private(set) var phase: Phase = .idle
    /// The words as heard.
    public private(set) var liveText = ""
    public private(set) var notice: String?
    public let engines: SpeechEngines
    @ObservationIgnored private let input: VoiceInput
    @ObservationIgnored private var listen: Task<Void, Never>?

    public init(engines: SpeechEngines, input: VoiceInput) {
        self.engines = engines
        self.input = input
    }

    public var engineLabel: String? { engines.lastEngine?.label }

    /// Starts listening. The engines warm up meanwhile.
    public func start() async {
        guard phase == .idle else { return }
        guard !engines.engines.isEmpty else {
            notice = "No speech engine is on. Turn one on in Settings."
            return
        }
        notice = nil
        liveText = ""
        phase = .listening
        Task { await self.engines.prepare() }
        do {
            try await input.start()
        } catch {
            notice = SpeechEngines.describe(error)
            phase = .idle
            await engines.close()
            return
        }
        input.beginTurn()
        listen = Task { [weak self, input] in
            for await e in input.events {
                guard let self else { return }
                if case .words(let t, _) = e { self.liveText = t }
                if case .failed(let why) = e { self.notice = why }
            }
        }
    }

    /// Stops and returns the engine's text, or nil when no engine wrote any.
    public func stop() async -> String? {
        guard phase == .listening else { return nil }
        phase = .transcribing
        let (clip, live) = await input.endTurn()
        input.stop()
        listen?.cancel()
        listen = nil
        let r = await engines.transcribe(clip, live: live)
        if r == nil { notice = "No engine wrote text: \(engines.lastFallback ?? "")" }
        await engines.close()
        phase = .idle
        if let t = r?.text { liveText = t }
        return r?.text
    }

    /// Stops and drops the words.
    public func cancel() async {
        guard phase == .listening else { return }
        input.stop()
        listen?.cancel()
        listen = nil
        await engines.close()
        liveText = ""
        phase = .idle
    }
}

/// The microphone and speaker together, as voice mode uses them.
public typealias VoiceIO = VoiceInput & VoiceSpeaker

/// What voice mode and dictation need from the app: settings, the
/// on-device model, the audio, and the connection hold for the background.
@MainActor
@Observable
public final class VoiceKit {
    public let settings: VoiceSettingsStore
    public let parakeet: ParakeetHost
    /// Makes the audio: `speaks` for voice mode, not for dictation.
    @ObservationIgnored public var makeAudio: @MainActor (_ speaks: Bool) -> VoiceIO = { VoiceAudio(speaks: $0) }
    /// Keeps a hab's connection through the background while voice mode is
    /// open (app.md 13.1, 12.1), and lets it go.
    @ObservationIgnored public var hold: @MainActor (_ habID: String, _ on: Bool) -> Void = { _, _ in }
    /// Apple's live recognizer runs here (26 and later).
    public let liveWords: Bool
    @ObservationIgnored private var observers: [NSObjectProtocol] = []

    public init(settings: VoiceSettingsStore? = nil, parakeet: ParakeetHost? = nil, liveWords: Bool? = nil) {
        self.settings = settings ?? VoiceSettingsStore()
        self.parakeet = parakeet ?? ParakeetHost(runtime: nil)
        self.liveWords = liveWords ?? LiveWords.isSupported
        #if os(iOS)
        observers = self.parakeet.observeSystem()
        #endif
    }

    /// The enabled engines for one session, in order.
    public func engines(connection: HabConnection?, session: String?, holder: String) -> SpeechEngines {
        var list: [SpeechEngine] = []
        for k in settings.value.enabledEngines {
            switch k {
            case .hab:
                list.append(HabSpeechEngine(hab: connection.map { SpeechAPI(connection: $0) }, session: session))
            case .parakeet:
                if parakeet.offered { list.append(ParakeetSpeechEngine(host: parakeet, holder: holder)) }
            case .apple:
                list.append(AppleSpeechEngine(supported: liveWords))
            }
        }
        return SpeechEngines(list)
    }

    /// Voice mode for a session.
    public func voiceModel(chat: ChatModel, connection: HabConnection?) -> VoiceModel {
        let audio = makeAudio(true)
        let m = VoiceModel(chat: chat, engines: engines(connection: connection, session: chat.target.session, holder: "voice"),
                           input: audio, speaker: audio, settings: settings.value) { params in
            guard let connection else { throw ConnectionClosed(detail: "no connection") }
            _ = try await connection.call(HabMethods.approvalAnswer, params)
        }
        return m
    }

    /// Dictation into the composer or a pane.
    public func dictation(connection: HabConnection?, session: String?) -> DictationModel {
        DictationModel(engines: engines(connection: connection, session: session, holder: "dictation"), input: makeAudio(false))
    }
}

extension EnvironmentValues {
    /// Voice mode and dictation; nil hides their controls.
    @Entry public var voiceKit: VoiceKit? = nil
}
