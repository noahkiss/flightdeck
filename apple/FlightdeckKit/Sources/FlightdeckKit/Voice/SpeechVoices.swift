import AVFoundation
import Foundation

/// The voices voice mode can speak with (app.md 13.7, "Spoken voice"): the
/// system's installed voices, on the device. Automatic picks the best one for
/// the language: premium, then enhanced, then default.
public enum SpeechVoices {
    /// One installed voice, as the picker shows it.
    public struct Voice: Identifiable, Equatable, Sendable {
        public var id: String
        public var name: String
        /// BCP 47, such as "en-US".
        public var language: String
        /// 3 premium, 2 enhanced, 1 default.
        public var quality: Int

        public init(id: String, name: String, language: String, quality: Int) {
            self.id = id
            self.name = name
            self.language = language
            self.quality = quality
        }

        public var qualityText: String {
            switch quality {
            case 3: return "Premium"
            case 2: return "Enhanced"
            default: return "Default"
            }
        }
    }

    /// The best voice for a language: the highest quality, then the exact
    /// language and region over another region, then the name, so the pick
    /// never changes between runs.
    public static func best(_ voices: [Voice], language: String) -> Voice? {
        candidates(voices, language: language).first
    }

    /// The voices for a language's code ("en" for "en-GB"), best first.
    public static func candidates(_ voices: [Voice], language: String) -> [Voice] {
        let code = Self.code(language)
        return voices
            .filter { Self.code($0.language) == code }
            .sorted { a, b in
                if a.quality != b.quality { return a.quality > b.quality }
                let ae = a.language == language, be = b.language == language
                if ae != be { return ae }
                if a.name != b.name { return a.name < b.name }
                return a.id < b.id
            }
    }

    /// The voice to speak with: the chosen one while it is installed, else the best.
    public static func pick(_ voices: [Voice], chosen: String?, language: String) -> Voice? {
        if let chosen, let v = voices.first(where: { $0.id == chosen }) { return v }
        return best(voices, language: language)
    }

    static func code(_ language: String) -> String {
        String(language.split(whereSeparator: { $0 == "-" || $0 == "_" }).first ?? "").lowercased()
    }

    // MARK: - The system's voices

    /// The language voice mode speaks: the device's, when it has voices, else US English.
    public static var language: String {
        let current = AVSpeechSynthesisVoice.currentLanguageCode()
        return installed().contains { code($0.language) == code(current) } ? current : "en-US"
    }

    /// Installed voices, without novelty and personal voices.
    public static func installed() -> [Voice] {
        AVSpeechSynthesisVoice.speechVoices().compactMap { v in
            if v.voiceTraits.contains(.isNoveltyVoice) || v.voiceTraits.contains(.isPersonalVoice) { return nil }
            let q: Int
            switch v.quality {
            case .premium: q = 3
            case .enhanced: q = 2
            default: q = 1
            }
            return Voice(id: v.identifier, name: v.name, language: v.language, quality: q)
        }
    }

    /// The system voice for the owner's choice; nil leaves the system default.
    public static func system(chosen: String?) -> AVSpeechSynthesisVoice? {
        let lang = language
        if let v = pick(installed(), chosen: chosen, language: lang), let s = AVSpeechSynthesisVoice(identifier: v.id) { return s }
        return AVSpeechSynthesisVoice(language: lang)
    }
}

/// Plays a short sample in Settings. Like voice mode, it plays with the silent
/// switch on, and gives other audio back when it ends.
@MainActor
final class VoicePreviewer: NSObject, AVSpeechSynthesizerDelegate {
    private let synthesizer = AVSpeechSynthesizer()
    private var ownsSession = false

    override init() {
        super.init()
        synthesizer.delegate = self
    }

    func play(_ text: String, voice: AVSpeechSynthesisVoice?) {
        synthesizer.stopSpeaking(at: .immediate)
        #if os(iOS)
        // Voice mode's microphone owns the session while it runs.
        if VoiceAudio.sessionUsers == 0 {
            let s = AVAudioSession.sharedInstance()
            try? s.setCategory(.playback, mode: .spokenAudio, options: [.duckOthers])
            try? s.setActive(true)
            ownsSession = true
        }
        #endif
        let u = AVSpeechUtterance(string: text)
        u.voice = voice
        synthesizer.speak(u)
    }

    private func release() {
        #if os(iOS)
        guard ownsSession, !synthesizer.isSpeaking, VoiceAudio.sessionUsers == 0 else { return }
        ownsSession = false
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        #endif
    }

    nonisolated func speechSynthesizer(_ synthesizer: AVSpeechSynthesizer, didFinish utterance: AVSpeechUtterance) {
        Task { @MainActor in self.release() }
    }

    nonisolated func speechSynthesizer(_ synthesizer: AVSpeechSynthesizer, didCancel utterance: AVSpeechUtterance) {
        Task { @MainActor in self.release() }
    }
}
