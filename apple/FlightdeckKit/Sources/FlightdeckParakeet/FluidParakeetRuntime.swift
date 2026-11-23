import FlightdeckKit
import FluidAudio
import Foundation

/// On-device Parakeet TDT 0.6B v2 (English) through FluidAudio's Core ML
/// port, as the spike measured it (`swift/Spikes/ParakeetSpike`). The model
/// downloads on first use from Hugging Face into FluidAudio's cache; the
/// app's `ParakeetHost` decides when it is loaded.
///
/// Every disk or model step is `@concurrent`: it walks or reads hundreds of
/// MB, or runs the model, so it never starts on the caller's actor (the main
/// actor in the app).
public final class FluidParakeetRuntime: ParakeetRuntime, Sendable {
    private let state = State()

    public init() {}

    private actor State {
        var manager: AsrManager?
        func set(_ m: AsrManager?) { manager = m }
    }

    private var directory: URL { AsrModels.defaultCacheDirectory(for: .v2) }

    @concurrent public func isDownloaded() async -> Bool {
        let here = AsrModels.modelsExist(at: directory, version: .v2)
        if here { excludeFromBackup() }
        return here
    }

    public func download(progress: @escaping @Sendable (Double) -> Void) async throws {
        _ = try await AsrModels.download(version: .v2) { p in progress(p.fractionCompleted) }
        excludeFromBackup()
    }

    /// The model (about 450 MB) downloads again on demand: keep it out of the device backup.
    private func excludeFromBackup() {
        var url = directory
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        try? url.setResourceValues(values)
    }

    @concurrent public func removeDownload() async throws {
        await unload()
        if FileManager.default.fileExists(atPath: directory.path) {
            try FileManager.default.removeItem(at: directory)
        }
    }

    @concurrent public func load() async throws {
        if await state.manager != nil { return }
        let models = try await AsrModels.load(from: directory, version: .v2)
        let manager = AsrManager(config: .default)
        try await manager.loadModels(models)
        await state.set(manager)
    }

    public func unload() async {
        guard let m = await state.manager else { return }
        await state.set(nil)
        await m.cleanup()
    }

    @concurrent public func transcribe(_ samples: [Float]) async throws -> String {
        guard let m = await state.manager else {
            throw SpeechEngineError.unavailable("the on-device model is not loaded", sticky: false)
        }
        var decoder = try TdtDecoderState(decoderLayers: AsrModelVersion.v2.decoderLayers)
        return try await m.transcribe(samples, decoderState: &decoder).text
    }
}
