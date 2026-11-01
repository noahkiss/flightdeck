import Foundation

/// What the microphone side tells voice mode and dictation.
public enum VoiceInputEvent: Sendable, Equatable {
    /// The level of the latest audio, in dBFS, with a monotonic time in seconds.
    case level(Float, at: Double)
    /// The live recognizer's text for the current turn changed.
    case words(String, at: Double)
    /// A phone call or alarm began (true) or ended (false).
    case interrupted(Bool)
    /// The audio route changed; the input restarted.
    case routeChanged
    /// The input stopped with an error.
    case failed(String)
}

/// The microphone, the live recognizer and the turn buffer. The real one is
/// `VoiceAudio`; tests drive a fake.
@MainActor
public protocol VoiceInput: AnyObject {
    var events: AsyncStream<VoiceInputEvent> { get }
    /// Whether Apple's live recognizer runs on this system.
    var hasLiveWords: Bool { get }
    func start() async throws
    func stop()
    /// Muted: no audio is kept and no level or words are reported.
    func setMuted(_ muted: Bool)
    /// A turn starts now: keep a short lead-in and everything after it.
    func beginTurn()
    /// Ends the turn: its audio and the live recognizer's text.
    func endTurn() async -> (clip: SpeechClip, live: String)
}

/// Reads text aloud and plays the small cues. The real one is `VoiceAudio`.
@MainActor
public protocol VoiceSpeaker: AnyObject {
    /// Reads `text`; returns true when it finished, false when stopped.
    func speak(_ text: String) async -> Bool
    func stopSpeaking()
    var isSpeaking: Bool { get }
    /// The soft tone that says the agent is still working.
    func tone()
    /// The chime for another session that needs the owner.
    func chime()
}
