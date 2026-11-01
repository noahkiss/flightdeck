import Foundation

/// Finds where the owner starts and stops talking (app.md 13.1): from the
/// audio level and from new live words. A turn ends after `silence` with no
/// speech once speech began. Times are seconds on any monotonic clock.
public struct TurnDetector: Sendable {
    public enum Event: Equatable, Sendable {
        /// Speech began: stop playback (barge-in) and start a turn.
        case speechStarted
        /// Enough silence after speech: the turn ends.
        case turnEnded
    }

    /// Silence that ends a turn (default 1.2 s).
    public var silence: Double
    /// Level in dBFS above which a frame counts as speech.
    public var threshold: Float
    /// Speech must last this long to start a turn, so a click does not.
    public var minSpeech: Double = 0.15
    /// While the app speaks, speech needs this much more level: the echo
    /// canceller leaves some of the app's own voice.
    public var bargeInMargin: Float = 10

    public private(set) var inSpeech = false
    private var loudSince: Double?
    private var lastSpeech: Double?

    public init(silence: Double = 1.2, threshold: Float = -38) {
        self.silence = silence
        self.threshold = threshold
    }

    /// One audio level reading. `speaking`: the app is reading aloud now.
    public mutating func level(_ db: Float, at t: Double, speaking: Bool = false) -> Event? {
        let need = threshold + (speaking ? bargeInMargin : 0)
        if db >= need {
            if inSpeech {
                lastSpeech = t
                return nil
            }
            let since = loudSince ?? t
            loudSince = since
            if t - since >= minSpeech {
                return start(at: t)
            }
            return nil
        }
        loudSince = nil
        return checkEnd(at: t)
    }

    /// The live recognizer heard new words: that is speech, whatever the level.
    /// While the app speaks, words do not start a turn: they may be its own voice.
    public mutating func words(at t: Double, speaking: Bool = false) -> Event? {
        if inSpeech {
            lastSpeech = t
            return nil
        }
        return speaking ? nil : start(at: t)
    }

    /// A clock tick with no reading, so silence ends a turn on time.
    public mutating func tick(at t: Double) -> Event? {
        checkEnd(at: t)
    }

    /// Forget the current turn (after a tap ended it, or on mute).
    public mutating func reset() {
        inSpeech = false
        loudSince = nil
        lastSpeech = nil
    }

    private mutating func start(at t: Double) -> Event {
        inSpeech = true
        lastSpeech = t
        loudSince = nil
        return .speechStarted
    }

    private mutating func checkEnd(at t: Double) -> Event? {
        guard inSpeech, let last = lastSpeech, t - last >= silence else { return nil }
        reset()
        return .turnEnded
    }
}

/// RMS level of samples in dBFS; silence is -160.
public func audioLevel(_ samples: UnsafeBufferPointer<Float>) -> Float {
    guard !samples.isEmpty else { return -160 }
    var sum: Float = 0
    for s in samples { sum += s * s }
    let rms = (sum / Float(samples.count)).squareRoot()
    return rms > 0 ? max(-160, 20 * log10(rms)) : -160
}
