import Foundation
import Observation

/// The owner's voice settings (app.md 13, voice.md "Where transcription runs").
public struct VoiceSettings: Codable, Equatable, Sendable {
    /// Engines in their fixed order; each can be turned off.
    public var habEngine = true
    public var parakeetEngine = false
    public var appleEngine = true
    /// Append the request for a spoken summary to each prompt (voice.md Decisions).
    public var askSummary = true
    /// "stop", "repeat", "allow", "deny", "end voice" act when said alone.
    public var spokenCommands = true
    /// The owner says "send" or taps before a turn goes to the agent.
    public var confirmBeforeSend = false
    /// Speech stops playback; off, only a tap does.
    public var bargeIn = true
    /// A noisy room: speech must be louder to start a turn or a barge-in.
    public var noisyRoom = false
    /// Silence that ends a turn, in seconds.
    public var silence: Double = 1.2

    public init() {}

    /// The enabled engines, in the fallback order.
    public var enabledEngines: [SpeechEngineKind] {
        SpeechEngineKind.allCases.filter { isOn($0) }
    }

    public func isOn(_ k: SpeechEngineKind) -> Bool {
        switch k {
        case .hab: return habEngine
        case .parakeet: return parakeetEngine
        case .apple: return appleEngine
        }
    }

    public mutating func set(_ k: SpeechEngineKind, _ on: Bool) {
        switch k {
        case .hab: habEngine = on
        case .parakeet: parakeetEngine = on
        case .apple: appleEngine = on
        }
    }

    /// The level that counts as speech, in dBFS.
    public var threshold: Float { noisyRoom ? -28 : -38 }

    /// A soft tone while the agent works (app.md 13.1).
    public static let waitTone: Double = 20
    /// Voice mode ends after this long with no speech and no turn running (app.md 13.1).
    public static let idleLimit: Double = 600
}

/// The voice settings, kept in the app's defaults.
@MainActor
@Observable
public final class VoiceSettingsStore {
    public static let key = "FlightdeckVoiceSettings"
    @ObservationIgnored private let defaults: UserDefaults

    public var value: VoiceSettings {
        didSet {
            if let d = try? JSONEncoder().encode(value) { defaults.set(d, forKey: Self.key) }
        }
    }

    public init(defaults: UserDefaults = .standard) {
        self.defaults = defaults
        if let d = defaults.data(forKey: Self.key), let v = try? JSONDecoder().decode(VoiceSettings.self, from: d) {
            value = v
        } else {
            value = VoiceSettings()
        }
    }
}
