import AVFoundation
import FluidAudio
import Foundation
import Speech

/// One transcription by one engine.
struct EngineResult {
    let engine: String
    let text: String
    let audioSeconds: Double
    let seconds: Double
    /// Processing time divided by audio time. Below 1 is faster than real time.
    var realTimeFactor: Double { audioSeconds > 0 ? seconds / audioSeconds : 0 }
}

/// Parakeet TDT 0.6B v2 (English) through FluidAudio's Core ML port.
final class ParakeetEngine {
    static let name = "Parakeet v2"
    static let modelRepo = "FluidInference/parakeet-tdt-0.6b-v2-coreml"

    private var manager: AsrManager?
    private var modelsDirectory: URL?

    var isLoaded: Bool { manager != nil }

    static func isDownloaded() -> Bool {
        AsrModels.modelsExist(at: AsrModels.defaultCacheDirectory(for: .v2), version: .v2)
    }

    /// Downloads the model files if they are missing. Returns elapsed seconds.
    func download(progress: @escaping @Sendable (DownloadProgress) -> Void) async throws -> Double {
        let start = Date()
        modelsDirectory = try await AsrModels.download(version: .v2, progressHandler: progress)
        return Date().timeIntervalSince(start)
    }

    /// Loads the Core ML models into memory. Returns elapsed seconds.
    func load() async throws -> Double {
        let directory = modelsDirectory ?? AsrModels.defaultCacheDirectory(for: .v2)
        let start = Date()
        let models = try await AsrModels.load(from: directory, version: .v2)
        let manager = AsrManager(config: .default)
        try await manager.loadModels(models)
        self.manager = manager
        return Date().timeIntervalSince(start)
    }

    func unload() async {
        guard let manager else { return }
        self.manager = nil
        await manager.cleanup()
    }

    func transcribe(_ clip: Clip) async throws -> EngineResult {
        guard let manager else { throw SpikeError("Parakeet is not loaded") }
        var state = try TdtDecoderState(decoderLayers: AsrModelVersion.v2.decoderLayers)
        let start = Date()
        let result = try await manager.transcribe(clip.samples, decoderState: &state)
        let seconds = Date().timeIntervalSince(start)
        return EngineResult(engine: Self.name, text: result.text, audioSeconds: clip.duration, seconds: seconds)
    }
}

/// Apple SpeechAnalyzer (iOS 26+). The model runs in a system process, not in this app.
enum AppleEngine {
    static let name = "Apple SpeechAnalyzer"
    static let locale = Locale(identifier: "en-US")

    static var isSupported: Bool {
        if #available(iOS 26, *) { return true }
        return false
    }

    /// Makes sure the on-device speech asset for the locale is installed.
    /// Returns a short status line.
    static func prepare() async throws -> String {
        guard #available(iOS 26, *) else { return "Needs iOS 26 or later" }
        let module = try await makeModule()
        if let request = try await AssetInventory.assetInstallationRequest(supporting: [module.module]) {
            try await request.downloadAndInstall()
            return "Speech asset installed (\(module.kind))"
        }
        return "Speech asset ready (\(module.kind))"
    }

    static func transcribe(_ clip: Clip) async throws -> EngineResult {
        guard #available(iOS 26, *) else { throw SpikeError("SpeechAnalyzer needs iOS 26 or later") }
        let module = try await makeModule()
        if let request = try await AssetInventory.assetInstallationRequest(supporting: [module.module]) {
            try await request.downloadAndInstall()
        }
        let start = Date()
        let collector = Task { try await module.collect() }
        let analyzer = SpeechAnalyzer(modules: [module.module])
        let file = try AVAudioFile(forReading: clip.fileURL)
        if let end = try await analyzer.analyzeSequence(from: file) {
            try await analyzer.finalizeAndFinish(through: end)
        } else {
            await analyzer.cancelAndFinishNow()
        }
        let text = try await collector.value
        let seconds = Date().timeIntervalSince(start)
        return EngineResult(engine: name, text: text, audioSeconds: clip.duration, seconds: seconds)
    }

    /// SpeechTranscriber where the device supports it, else DictationTranscriber.
    @available(iOS 26, *)
    private struct Module {
        let module: any SpeechModule
        let kind: String
        let collect: () async throws -> String
    }

    @available(iOS 26, *)
    private static func makeModule() async throws -> Module {
        if SpeechTranscriber.isAvailable,
           let locale = await SpeechTranscriber.supportedLocale(equivalentTo: locale) {
            let transcriber = SpeechTranscriber(locale: locale, preset: .transcription)
            return Module(module: transcriber, kind: "SpeechTranscriber") {
                var text = ""
                for try await result in transcriber.results where result.isFinal {
                    text += String(result.text.characters)
                }
                return text.trimmingCharacters(in: .whitespacesAndNewlines)
            }
        }
        guard let locale = await DictationTranscriber.supportedLocale(equivalentTo: locale) else {
            throw SpikeError("No Apple speech model supports \(locale.identifier)")
        }
        let transcriber = DictationTranscriber(locale: locale, preset: .shortDictation)
        return Module(module: transcriber, kind: "DictationTranscriber") {
            var text = ""
            for try await result in transcriber.results where result.isFinal {
                text += String(result.text.characters)
            }
            return text.trimmingCharacters(in: .whitespacesAndNewlines)
        }
    }
}
