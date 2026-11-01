import Foundation
import HabAPI
import Observation

/// The speech-to-text engines, in the fallback order (voice.md "Where
/// transcription runs").
public enum SpeechEngineKind: String, CaseIterable, Codable, Sendable, Identifiable {
    case hab
    case parakeet
    case apple

    public var id: String { rawValue }

    /// The footer of the voice view names the engine that wrote the text.
    public var label: String {
        switch self {
        case .hab: return "Parakeet on the hab"
        case .parakeet: return "Parakeet on this device"
        case .apple: return "Apple speech"
        }
    }
}

/// One recorded turn: 16 kHz mono samples.
public struct SpeechClip: Sendable, Equatable {
    public static let sampleRate = 16_000

    public var samples: [Float]

    public init(samples: [Float]) {
        self.samples = samples
    }

    public var duration: Double { Double(samples.count) / Double(Self.sampleRate) }

    /// Little-endian 16-bit PCM, the hab's fallback format.
    public var pcm16: Data {
        var d = Data(capacity: samples.count * 2)
        for s in samples {
            let v = Int16(max(-1, min(1, s)) * Float(Int16.max))
            withUnsafeBytes(of: v.littleEndian) { d.append(contentsOf: $0) }
        }
        return d
    }
}

/// Why an engine cannot write this turn's text. Every case moves the turn to
/// the next enabled engine.
public enum SpeechEngineError: Error, Equatable, Sendable, CustomStringConvertible {
    /// The engine cannot serve now. `sticky`: not for the rest of this voice
    /// mode or dictation (no `speech` capability, no model, an older system).
    case unavailable(String, sticky: Bool)

    public var description: String {
        switch self {
        case .unavailable(let hint, _): return hint
        }
    }
}

/// What `prepare` found.
public enum SpeechEngineStatus: Equatable, Sendable {
    case ready
    /// Usable soon: a download or load runs; the hint says how far.
    case preparing(String)
    /// The last download or load failed (the hab's `error`); the next
    /// prepare tries again.
    case failed(String)
}

/// One speech-to-text engine. Voice mode and dictation open it with
/// `prepare`, send it one clip per turn, and `close` it when they close.
@MainActor
public protocol SpeechEngine: AnyObject {
    var kind: SpeechEngineKind { get }
    /// Warms the engine up. Throws `SpeechEngineError.unavailable` to be skipped.
    func prepare() async throws -> SpeechEngineStatus
    /// The text of one turn. `live` is what Apple's live recognizer heard.
    func transcribe(_ clip: SpeechClip, live: String) async throws -> String
    /// Voice mode or dictation closed: give back what `prepare` took.
    func close() async
}

/// The engines the owner enabled, in order, and the fallback between them
/// (voice.md, Rules): an engine that is unavailable sends the turn to the
/// next one; a sticky refusal skips it until the engines close.
@MainActor
@Observable
public final class SpeechEngines {
    public let engines: [SpeechEngine]
    /// Engines skipped until close, with the reason.
    public private(set) var skipped: [SpeechEngineKind: String] = [:]
    /// What each engine's `prepare` said, for the voice view.
    public private(set) var status: [SpeechEngineKind: SpeechEngineStatus] = [:]
    /// The engine that wrote the last text, for the footer.
    public private(set) var lastEngine: SpeechEngineKind?
    /// Why the engines before `lastEngine` did not write it.
    public private(set) var lastFallback: String?

    public init(_ engines: [SpeechEngine]) {
        self.engines = engines
    }

    /// The engine the next turn tries first.
    public var first: SpeechEngineKind? {
        engines.first { skipped[$0.kind] == nil }?.kind
    }

    /// Set by `close`; a `prepare` that finishes after it gives back what it took.
    @ObservationIgnored private var closed = false

    /// Prepares every engine at once; a sticky refusal skips it. An engine
    /// whose prepare ends after `close` is closed again, so a late Parakeet
    /// load is released at once.
    public func prepare() async {
        closed = false
        await withTaskGroup(of: Void.self) { g in
            for e in engines {
                g.addTask { @MainActor in await self.prepare(e) }
            }
        }
    }

    private func prepare(_ e: SpeechEngine) async {
        let result: Result<SpeechEngineStatus, Error>
        do {
            result = .success(try await e.prepare())
        } catch {
            result = .failure(error)
        }
        if closed {
            await e.close()
            return
        }
        switch result {
        case .success(let s):
            status[e.kind] = s
        case .failure(SpeechEngineError.unavailable(let hint, let sticky)):
            status[e.kind] = nil
            if sticky { skipped[e.kind] = hint }
        case .failure:
            status[e.kind] = nil
        }
    }

    /// The text of one turn and the engine that wrote it, or nil when every
    /// engine failed. Any failure moves the turn to the next engine.
    public func transcribe(_ clip: SpeechClip, live: String) async -> (text: String, engine: SpeechEngineKind)? {
        var reasons: [String] = []
        for e in engines where skipped[e.kind] == nil {
            do {
                let text = try await e.transcribe(clip, live: live).trimmingCharacters(in: .whitespacesAndNewlines)
                lastEngine = e.kind
                lastFallback = reasons.isEmpty ? nil : reasons.joined(separator: "; ")
                return (text, e.kind)
            } catch let SpeechEngineError.unavailable(hint, sticky) {
                if sticky { skipped[e.kind] = hint }
                reasons.append("\(e.kind.label): \(hint)")
            } catch is CancellationError {
                // Voice mode or dictation closed: no other engine runs.
                return nil
            } catch {
                reasons.append("\(e.kind.label): \(Self.describe(error))")
            }
        }
        lastFallback = reasons.isEmpty ? "no speech engine is on" : reasons.joined(separator: "; ")
        return nil
    }

    /// Closes every engine. It does not wait for a prepare in flight.
    public func close() async {
        closed = true
        for e in engines { await e.close() }
        skipped = [:]
        status = [:]
    }

    static func describe(_ error: Error) -> String {
        if let e = error as? RPCError { return e.data?.hint ?? e.message }
        return String(describing: error)
    }
}

// MARK: - The hab engine

/// Transcription on the session's hab (voice.md, the default): `speech.prepare`
/// on open, `speech.transcribe` per turn, Ogg Opus at about 24 kbit/s, or
/// 16-bit PCM when the hab does not take Opus or the encoder fails.
@MainActor
public final class HabSpeechEngine: SpeechEngine {
    public let kind = SpeechEngineKind.hab
    private let hab: SpeechHab?
    private let session: String?
    private let encode: (SpeechClip) throws -> Data
    private var last: SpeechStatus?

    /// `encode` makes the Ogg Opus clip; `OpusEncoder.oggOpus` by default.
    public init(hab: SpeechHab?, session: String?, encode: @escaping (SpeechClip) throws -> Data = { try OpusEncoder.oggOpus($0) }) {
        self.hab = hab
        self.session = session
        self.encode = encode
    }

    public func prepare() async throws -> SpeechEngineStatus {
        guard let hab else { throw SpeechEngineError.unavailable("no hab", sticky: true) }
        guard await hab.isWelcomed() else {
            // Not connected yet: ask again on the next turn.
            throw SpeechEngineError.unavailable("the hab is not connected yet", sticky: false)
        }
        guard await hab.hasSpeech() else {
            throw SpeechEngineError.unavailable("this hab has no speech engine", sticky: true)
        }
        let r: SpeechStatus
        do {
            r = try await hab.prepare()
        } catch let e as RPCError where e.kind == .unavailable {
            throw SpeechEngineError.unavailable(e.data?.hint ?? "the hab's speech engine is off", sticky: true)
        }
        last = r
        return Self.status(r)
    }

    static func status(_ r: SpeechStatus) -> SpeechEngineStatus {
        switch r.state {
        case .ready, .loading: return .ready
        case _ where r.error?.isEmpty == false: return .failed(r.error!)
        case .downloading: return .preparing(r.progress.map { "downloading \($0)%" } ?? "downloading")
        default: return .preparing("the model downloads on first use")
        }
    }

    public func transcribe(_ clip: SpeechClip, live: String) async throws -> String {
        guard let hab else { throw SpeechEngineError.unavailable("no hab", sticky: true) }
        if last == nil || Self.status(last!) != .ready {
            // A download may have finished since the last turn.
            _ = try await prepare()
        }
        switch last.map(Self.status) {
        case .preparing(let hint)?, .failed(let hint)?:
            throw SpeechEngineError.unavailable(hint, sticky: false)
        default:
            break
        }
        guard clip.duration <= SpeechLimits.maxClipSeconds else {
            throw SpeechEngineError.unavailable("the turn is longer than 120 s", sticky: false)
        }
        let params = try body(clip)
        do {
            return try await hab.transcribe(params).text
        } catch let e as RPCError where e.kind == .unavailable {
            last = nil
            throw SpeechEngineError.unavailable(e.data?.hint ?? "the hab's speech engine is unavailable", sticky: false)
        }
    }

    /// The request body: Opus when the hab takes it, else PCM; at most 1 MiB.
    func body(_ clip: SpeechClip) throws -> SpeechTranscribeParams {
        let formats = last?.formats ?? [SpeechFormat.pcm16]
        if formats.contains(.opus), let data = try? encode(clip), !data.isEmpty, data.count <= SpeechLimits.maxClipBytes {
            return SpeechTranscribeParams(audio: data, format: .opus, language: "en", session: session)
        }
        let pcm = clip.pcm16
        guard pcm.count <= SpeechLimits.maxClipBytes else {
            throw SpeechEngineError.unavailable("the turn is over 1 MiB as PCM", sticky: false)
        }
        return SpeechTranscribeParams(audio: pcm, format: .pcm16, language: "en", sampleRate: SpeechClip.sampleRate, session: session)
    }

    public func close() async {
        last = nil
    }
}

// MARK: - Apple speech

/// Apple's engine (voice.md, the last fallback): the text is what the live
/// recognizer heard during the turn, so it costs nothing more.
@MainActor
public final class AppleSpeechEngine: SpeechEngine {
    public let kind = SpeechEngineKind.apple
    private let supported: Bool

    /// `supported`: the live recognizer runs on this system.
    public init(supported: Bool) {
        self.supported = supported
    }

    public func prepare() async throws -> SpeechEngineStatus {
        guard supported else {
            throw SpeechEngineError.unavailable("Apple speech needs a newer system", sticky: true)
        }
        return .ready
    }

    public func transcribe(_ clip: SpeechClip, live: String) async throws -> String {
        guard supported else { throw SpeechEngineError.unavailable("Apple speech needs a newer system", sticky: true) }
        let t = live.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty else { throw SpeechEngineError.unavailable("no words heard", sticky: false) }
        return t
    }

    public func close() async {}
}

// MARK: - On-device Parakeet

/// Parakeet on the device, through the shared `ParakeetHost`, which owns the
/// load and unload rules.
@MainActor
public final class ParakeetSpeechEngine: SpeechEngine {
    public let kind = SpeechEngineKind.parakeet
    private let host: ParakeetHost
    private let holder: String

    /// `holder` names who holds the model: one token per voice mode or dictation.
    public init(host: ParakeetHost, holder: String) {
        self.host = host
        self.holder = holder
    }

    public func prepare() async throws -> SpeechEngineStatus {
        try await host.acquire(holder)
        return host.isLoaded ? .ready : .preparing("loading")
    }

    public func transcribe(_ clip: SpeechClip, live: String) async throws -> String {
        // A turn can end before `prepare` held the model (a slow hab first).
        if !closed, !host.holders.contains(holder) { try await host.acquire(holder) }
        return try await host.transcribe(clip.samples, for: holder)
    }

    private var closed = false

    public func close() async {
        closed = true
        await host.release(holder)
    }
}
