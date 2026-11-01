import Foundation
import HabAPI

// The hab's speech methods (voice.md, "API addition"), written by hand until
// the schema carries them. Everything that calls the hab goes through
// `SpeechAPI`, so moving to the generated types changes this file only:
// replace these types with typealiases to the generated ones and point
// `SpeechAPI` at `HabMethods.speechPrepare` and `HabMethods.speechTranscribe`.

/// The capability a hab names in its welcome when it runs the speech engine.
public let speechCapability = "speech"

/// The engine's state on the hab. Open: an unknown value is kept.
public struct SpeechEngineState: RawRepresentable, Codable, Sendable, Hashable {
    public var rawValue: String
    public init(rawValue: String) { self.rawValue = rawValue }
    public static let absent = SpeechEngineState(rawValue: "absent")
    public static let downloading = SpeechEngineState(rawValue: "downloading")
    public static let loading = SpeechEngineState(rawValue: "loading")
    public static let ready = SpeechEngineState(rawValue: "ready")
}

/// An audio format the hab accepts. Open.
public struct SpeechAudioFormat: RawRepresentable, Codable, Sendable, Hashable {
    public var rawValue: String
    public init(rawValue: String) { self.rawValue = rawValue }
    /// Ogg Opus (RFC 7845), mono.
    public static let opus = SpeechAudioFormat(rawValue: "opus")
    /// Little-endian 16-bit mono PCM, 16 kHz unless `sample_rate` says otherwise.
    public static let pcm16 = SpeechAudioFormat(rawValue: "pcm16")
}

public struct SpeechPrepareParams: Codable, Sendable, Hashable {
    public init() {}
}

public struct SpeechPrepareResult: Codable, Sendable, Hashable {
    public var state: SpeechEngineState
    /// Whole percent, while downloading.
    public var progress: Int?
    public var model: String?
    public var formats: [SpeechAudioFormat]?

    public init(state: SpeechEngineState, progress: Int? = nil, model: String? = nil, formats: [SpeechAudioFormat]? = nil) {
        self.state = state
        self.progress = progress
        self.model = model
        self.formats = formats
    }
}

public struct SpeechTranscribeParams: Codable, Sendable, Hashable {
    /// The clip, base64.
    public var audio: String
    public var format: SpeechAudioFormat
    /// For `pcm16`; the hab's default is 16000.
    public var sampleRate: Int?
    /// Whose vocabulary the hab applies.
    public var session: String?
    /// Default `en`.
    public var language: String?

    enum CodingKeys: String, CodingKey {
        case audio, format, session, language
        case sampleRate = "sample_rate"
    }

    public init(audio: String, format: SpeechAudioFormat, sampleRate: Int? = nil, session: String? = nil, language: String? = nil) {
        self.audio = audio
        self.format = format
        self.sampleRate = sampleRate
        self.session = session
        self.language = language
    }
}

public struct SpeechTranscribeResult: Codable, Sendable, Hashable {
    public var text: String
    public var audioMs: Int?
    public var decodeMs: Int?
    /// The text before the vocabulary fix-up, when the fix-up changed anything.
    public var raw: String?

    enum CodingKeys: String, CodingKey {
        case text, raw
        case audioMs = "audio_ms"
        case decodeMs = "decode_ms"
    }

    public init(text: String, audioMs: Int? = nil, decodeMs: Int? = nil, raw: String? = nil) {
        self.text = text
        self.audioMs = audioMs
        self.decodeMs = decodeMs
        self.raw = raw
    }
}

/// The hab's limits on one clip (voice.md): 120 s and 1 MiB before base64.
public enum SpeechLimits {
    public static let maxClipSeconds: Double = 120
    public static let maxClipBytes = 1 << 20
}

/// The two speech calls on one hab connection: the seam the hab engine uses.
public protocol SpeechHab: Sendable {
    /// Whether the live connection's welcome names `speech`.
    func hasSpeech() async -> Bool
    func prepare() async throws -> SpeechPrepareResult
    func transcribe(_ p: SpeechTranscribeParams) async throws -> SpeechTranscribeResult
}

/// `SpeechHab` over a `HabConnection`.
public struct SpeechAPI: SpeechHab {
    public static let prepareMethod = "speech.prepare"
    public static let transcribeMethod = "speech.transcribe"
    /// A decode of a 120 s clip takes about 12 s on a busy machine (voice.md
    /// spike), and decodes wait in order; leave room for both.
    public static let transcribeTimeout: Duration = .seconds(60)

    public let connection: HabConnection

    public init(connection: HabConnection) {
        self.connection = connection
    }

    public func hasSpeech() async -> Bool {
        await connection.has(capability: speechCapability)
    }

    public func prepare() async throws -> SpeechPrepareResult {
        try await connection.call(named: Self.prepareMethod, SpeechPrepareParams())
    }

    public func transcribe(_ p: SpeechTranscribeParams) async throws -> SpeechTranscribeResult {
        try await connection.call(named: Self.transcribeMethod, p, timeout: Self.transcribeTimeout)
    }
}
