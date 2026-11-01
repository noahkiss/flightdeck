import Foundation
import HabAPI

// The hab's speech methods (voice.md, "API addition"), through the generated
// types in swift/HabAPI: `speech.prepare` returns a `SpeechStatus`,
// `speech.transcribe` takes `SpeechTranscribeParams` and returns a
// `SpeechTranscript`.

/// The capability a hab names in its welcome when it runs the speech engine.
public let speechCapability = "speech"

/// The hab's limits on one clip (voice.md): 120 s and 1 MiB before base64.
public enum SpeechLimits {
    public static let maxClipSeconds: Double = 120
    public static let maxClipBytes = 1 << 20
}

/// The two speech calls on one hab connection: the seam the hab engine uses.
public protocol SpeechHab: Sendable {
    /// Whether the connection is online with a welcome.
    func isWelcomed() async -> Bool
    /// Whether the live connection's welcome names `speech`.
    func hasSpeech() async -> Bool
    func prepare() async throws -> SpeechStatus
    func transcribe(_ p: SpeechTranscribeParams) async throws -> SpeechTranscript
}

/// `SpeechHab` over a `HabConnection`.
public struct SpeechAPI: SpeechHab {
    /// A decode of a 120 s clip takes about 12 s on a busy machine (voice.md
    /// spike), and decodes wait in order; leave room for both.
    public static let transcribeTimeout: Duration = .seconds(60)

    public let connection: HabConnection

    public init(connection: HabConnection) {
        self.connection = connection
    }

    public func isWelcomed() async -> Bool {
        await connection.isWelcomed
    }

    public func hasSpeech() async -> Bool {
        await connection.has(capability: speechCapability)
    }

    public func prepare() async throws -> SpeechStatus {
        try await connection.call(HabMethods.speechPrepare, SpeechPrepareParams())
    }

    public func transcribe(_ p: SpeechTranscribeParams) async throws -> SpeechTranscript {
        try await connection.call(HabMethods.speechTranscribe, p, timeout: Self.transcribeTimeout)
    }
}
