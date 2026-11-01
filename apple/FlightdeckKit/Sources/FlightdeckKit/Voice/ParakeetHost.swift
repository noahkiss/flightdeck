import Foundation
import Observation

/// The on-device Parakeet model, behind a seam: the app links the Core ML
/// runtime (`FlightdeckParakeet`); tests use a fake.
public protocol ParakeetRuntime: AnyObject, Sendable {
    /// The model files are on the device.
    func isDownloaded() async -> Bool
    /// Downloads the model files; `progress` runs with 0...1.
    func download(progress: @escaping @Sendable (Double) -> Void) async throws
    /// Removes the model files.
    func removeDownload() async throws
    func load() async throws
    func unload() async
    /// 16 kHz mono samples to text.
    func transcribe(_ samples: [Float]) async throws -> String
}

/// Owns the on-device model and its rules (voice.md "On-device sizing"):
/// it loads only while voice mode or dictation holds it, and unloads when
/// the last holder lets go, on a memory warning, and when the app leaves the
/// foreground. After a memory warning it stays unloaded until every holder
/// has let go: the system is short of memory, so the turn goes to the next engine.
@MainActor
@Observable
public final class ParakeetHost {
    /// Who holds the model: voice mode, dictation.
    public private(set) var holders: Set<String> = []
    public private(set) var isLoaded = false
    public private(set) var isDownloaded = false
    /// 0...1 while a download runs.
    public private(set) var downloadProgress: Double?
    public private(set) var lastError: String?
    /// Unloaded by a memory warning while held.
    public private(set) var evicted = false
    public private(set) var isForeground = true
    /// How many loads ran, for tests and the log.
    public private(set) var loads = 0

    /// The device class offers the option (memory budget, voice.md) and the
    /// build links the runtime.
    public var offered: Bool { deviceOffered && runtime != nil }
    @ObservationIgnored private let deviceOffered: Bool
    private var runtime: ParakeetRuntime?
    @ObservationIgnored private var loading: Task<Void, Error>?

    public init(runtime: ParakeetRuntime?, offered: Bool = ParakeetHost.deviceOffers()) {
        self.runtime = runtime
        self.deviceOffered = offered
    }

    /// Links the runtime: the iOS app does this at launch.
    public func install(_ runtime: ParakeetRuntime) {
        guard self.runtime == nil else { return }
        self.runtime = runtime
    }

    /// The memory budget (voice.md, default 1 GB peak for the model): offered
    /// on devices with at least 6 GB of memory. The iPhone 15 Pro (8 GB) ran
    /// it well; smaller devices wait for a measurement.
    public nonisolated static func deviceOffers(physicalMemory: UInt64 = ProcessInfo.processInfo.physicalMemory) -> Bool {
        physicalMemory >= 6 * 1024 * 1024 * 1024 - 512 * 1024 * 1024
    }

    /// Reads whether the model is on the device.
    public func refresh() async {
        guard let runtime else { return }
        isDownloaded = await runtime.isDownloaded()
    }

    /// Downloads the model, for Settings.
    public func download() async {
        guard let runtime, downloadProgress == nil else { return }
        downloadProgress = 0
        lastError = nil
        do {
            try await runtime.download { [weak self] p in
                Task { @MainActor in
                    if self?.downloadProgress != nil { self?.downloadProgress = p }
                }
            }
            isDownloaded = await runtime.isDownloaded()
        } catch {
            lastError = String(describing: error)
        }
        downloadProgress = nil
    }

    /// Removes the model files, for Settings. Unloads first.
    public func removeDownload() async {
        guard let runtime else { return }
        await unloadNow()
        do {
            try await runtime.removeDownload()
        } catch {
            lastError = String(describing: error)
        }
        isDownloaded = await runtime.isDownloaded()
    }

    /// Voice mode or dictation opened: hold the model and load it.
    public func acquire(_ holder: String) async throws {
        guard offered, runtime != nil else {
            throw SpeechEngineError.unavailable("on-device Parakeet is not offered on this device", sticky: true)
        }
        holders.insert(holder)
        await refresh()
        guard isDownloaded else {
            throw SpeechEngineError.unavailable("the on-device model is not downloaded", sticky: true)
        }
        try await ensureLoaded()
    }

    /// Voice mode or dictation closed. The last holder unloads the model.
    public func release(_ holder: String) async {
        holders.remove(holder)
        guard holders.isEmpty else { return }
        evicted = false
        await unloadNow()
    }

    /// The system is short of memory: unload now.
    public func memoryWarning() async {
        if !holders.isEmpty { evicted = true }
        await unloadNow()
    }

    /// The app left or returned to the foreground. Voice mode may keep
    /// running in the background, but never with the model loaded.
    public func setForeground(_ foreground: Bool) async {
        isForeground = foreground
        if !foreground {
            await unloadNow()
        } else if !holders.isEmpty, !evicted, isDownloaded {
            try? await ensureLoaded()
        }
    }

    /// Transcribes running now; an unload waits for them.
    public private(set) var inFlight = 0
    @ObservationIgnored private var unloadWhenIdle = false

    /// One turn's text. Loads again after a return to the foreground.
    public func transcribe(_ samples: [Float], for holder: String) async throws -> String {
        guard let runtime, holders.contains(holder) else {
            throw SpeechEngineError.unavailable("the on-device model is not held yet", sticky: false)
        }
        try await ensureLoaded()
        inFlight += 1
        defer {
            inFlight -= 1
            if inFlight == 0, unloadWhenIdle {
                unloadWhenIdle = false
                // A new holder may have come meanwhile: keep the model unless
                // memory or the background asked for the unload.
                if holders.isEmpty || evicted || !isForeground {
                    Task { await self.unloadNow() }
                }
            }
        }
        return try await runtime.transcribe(samples)
    }

    private func ensureLoaded() async throws {
        if evicted {
            throw SpeechEngineError.unavailable("the on-device model was unloaded after a memory warning", sticky: true)
        }
        guard isForeground else {
            throw SpeechEngineError.unavailable("the on-device model does not run in the background", sticky: false)
        }
        // A deferred unload after the last release: a new holder keeps the
        // model. (Memory warnings and the background threw above.)
        if unloadWhenIdle { unloadWhenIdle = false }
        if isLoaded { return }
        if let l = loading {
            try await l.value
            return
        }
        guard let runtime else { return }
        let t = Task { @MainActor in
            try await runtime.load()
        }
        loading = t
        defer { loading = nil }
        do {
            try await t.value
        } catch {
            lastError = String(describing: error)
            throw SpeechEngineError.unavailable("the on-device model did not load: \(error)", sticky: true)
        }
        loads += 1
        // A release, a memory warning or the background may have come during the load.
        if holders.isEmpty || evicted || !isForeground {
            await runtime.unload()
            throw SpeechEngineError.unavailable("the on-device model was unloaded during its load", sticky: false)
        }
        isLoaded = true
    }

    private func unloadNow() async {
        loading?.cancel()
        guard isLoaded, let runtime else { return }
        if inFlight > 0 {
            // Never free the model under a running inference: unload after it.
            unloadWhenIdle = true
            return
        }
        isLoaded = false
        await runtime.unload()
    }
}

#if os(iOS)
import UIKit

extension ParakeetHost {
    /// Follows the system: memory warnings, and the app entering the
    /// background or returning. Keep the returned tokens alive.
    public func observeSystem() -> [NSObjectProtocol] {
        let nc = NotificationCenter.default
        return [
            nc.addObserver(forName: UIApplication.didReceiveMemoryWarningNotification, object: nil, queue: .main) { [weak self] _ in
                Task { @MainActor in await self?.memoryWarning() }
            },
            nc.addObserver(forName: UIApplication.didEnterBackgroundNotification, object: nil, queue: .main) { [weak self] _ in
                Task { @MainActor in await self?.setForeground(false) }
            },
            nc.addObserver(forName: UIApplication.willEnterForegroundNotification, object: nil, queue: .main) { [weak self] _ in
                Task { @MainActor in await self?.setForeground(true) }
            },
        ]
    }
}
#endif
