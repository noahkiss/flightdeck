import FluidAudio
import Foundation
import UIKit

/// Holds every measurement and drives the two engines. One instance per app run.
@MainActor
final class SpikeModel: ObservableObject {
    struct Run: Identifiable {
        let id = UUID()
        let label: String
        let audioSeconds: Double
        var parakeet: EngineResult?
        var apple: EngineResult?
        var parakeetError: String?
        var appleError: String?
    }

    struct PhraseResult: Identifiable {
        let id = UUID()
        let index: Int
        let reference: String
        let run: Run
        var parakeetScore: WordErrorRate.Score? {
            run.parakeet.map { WordErrorRate.score(reference: reference, hypothesis: $0.text) }
        }
        var appleScore: WordErrorRate.Score? {
            run.apple.map { WordErrorRate.score(reference: reference, hypothesis: $0.text) }
        }
    }

    struct Event: Identifiable {
        let id = UUID()
        let time: Date
        let text: String
    }

    // Model state
    @Published var modelStatus = "Not downloaded"
    @Published var downloadFraction: Double?
    @Published var downloadSeconds: Double?
    @Published var loadSeconds: [Double] = []
    @Published var isBusy = false
    @Published var appleStatus = AppleEngine.isSupported ? "Not prepared" : "Needs iOS 26 or later"

    // Memory
    @Published var footprint: UInt64 = 0
    @Published var peakFootprint: UInt64 = 0
    @Published var available: UInt64?
    @Published var memoryWarnings = 0

    // Measurements
    @Published var runs: [Run] = []
    @Published var phraseResults: [PhraseResult] = []
    @Published var phraseIndex = 0
    @Published var events: [Event] = []
    @Published var isRecording = false
    @Published var unloadInBackground = true

    let launchedAt = Date()
    private let parakeet = ParakeetEngine()
    private let recorder = Recorder()
    private var memoryTimer: Timer?
    private var wasBackgrounded = false

    var isModelLoaded: Bool { parakeet.isLoaded }

    init() {
        if ParakeetEngine.isDownloaded() { modelStatus = "Downloaded, not loaded" }
        sampleMemory()
        memoryTimer = Timer.scheduledTimer(withTimeInterval: 0.25, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.sampleMemory() }
        }
        log("Launched. \(Self.deviceLine)")
    }

    // MARK: Memory

    func sampleMemory() {
        guard let value = Memory.footprintBytes() else { return }
        footprint = value
        peakFootprint = max(peakFootprint, value)
        available = Memory.availableBytes()
    }

    func handleMemoryWarning() {
        memoryWarnings += 1
        log("Memory warning at \(Memory.megabytes(footprint)) footprint")
        Task { await unload(reason: "memory warning") }
    }

    func handleBackground() {
        wasBackgrounded = true
        log("Entered background at \(Memory.megabytes(footprint))")
        if unloadInBackground { Task { await unload(reason: "background") } }
    }

    func handleForeground() {
        guard wasBackgrounded else { return }
        wasBackgrounded = false
        log("Returned to foreground at \(Memory.megabytes(footprint))")
    }

    // MARK: Model

    func downloadAndLoad() async {
        guard !isBusy else { return }
        isBusy = true
        defer { isBusy = false }
        do {
            let cached = ParakeetEngine.isDownloaded()
            modelStatus = cached ? "Checking model files" : "Downloading"
            downloadFraction = 0
            let seconds = try await parakeet.download { progress in
                Task { @MainActor [weak self] in self?.applyProgress(progress) }
            }
            downloadFraction = nil
            if cached {
                log(String(format: "Model already on disk (check took %.1f s)", seconds))
            } else {
                downloadSeconds = seconds
                log(String(format: "Model downloaded and compiled in %.1f s", seconds))
            }
            modelStatus = "Loading"
            let loadTime = try await parakeet.load()
            loadSeconds.append(loadTime)
            modelStatus = "Loaded"
            sampleMemory()
            log(String(format: "Model loaded in %.2f s, footprint %@", loadTime, Memory.megabytes(footprint)))
        } catch {
            downloadFraction = nil
            modelStatus = "Failed: \(error.localizedDescription)"
            log("Model download or load failed: \(error.localizedDescription)")
        }
    }

    private func applyProgress(_ progress: DownloadProgress) {
        downloadFraction = progress.fractionCompleted
        switch progress.phase {
        case .listing: modelStatus = "Listing model files"
        case let .downloading(done, total): modelStatus = "Downloading file \(done) of \(total)"
        case let .compiling(name): modelStatus = "Compiling \(name)"
        }
    }

    func unload(reason: String) async {
        guard parakeet.isLoaded else { return }
        let before = footprint
        await parakeet.unload()
        modelStatus = "Downloaded, not loaded"
        try? await Task.sleep(for: .milliseconds(300))
        sampleMemory()
        log("Model unloaded (\(reason)): \(Memory.megabytes(before)) to \(Memory.megabytes(footprint))")
    }

    func prepareApple() async {
        do {
            appleStatus = "Preparing"
            appleStatus = try await AppleEngine.prepare()
        } catch {
            appleStatus = "Failed: \(error.localizedDescription)"
        }
    }

    // MARK: Recording

    func startRecording() {
        guard !isRecording, !isBusy else { return }
        Task {
            guard await Recorder.requestPermission() else {
                log("Microphone permission denied")
                return
            }
            do {
                try recorder.start()
                isRecording = true
            } catch {
                log("Recording failed: \(error.localizedDescription)")
            }
        }
    }

    /// Stops recording and transcribes. With a phrase index, scores against that phrase.
    func stopRecording(phrase: Int? = nil) {
        guard isRecording else { return }
        isRecording = false
        do {
            let clip = try recorder.stop()
            guard clip.duration > 0.2 else {
                log("Clip too short, ignored")
                return
            }
            Task { await transcribe(clip, label: phrase.map { "Phrase \($0 + 1)" } ?? "Clip \(runs.count + 1)", phrase: phrase) }
        } catch {
            log("Stop failed: \(error.localizedDescription)")
        }
    }

    func transcribe(_ clip: Clip, label: String, phrase: Int? = nil) async {
        isBusy = true
        defer { isBusy = false }
        var run = Run(label: label, audioSeconds: clip.duration)
        if parakeet.isLoaded {
            do { run.parakeet = try await parakeet.transcribe(clip) } catch { run.parakeetError = error.localizedDescription }
        } else {
            run.parakeetError = "Model not loaded"
        }
        sampleMemory()
        if AppleEngine.isSupported {
            do { run.apple = try await AppleEngine.transcribe(clip) } catch { run.appleError = error.localizedDescription }
        } else {
            run.appleError = "Needs iOS 26 or later"
        }
        runs.append(run)
        if let phrase {
            phraseResults.removeAll { $0.index == phrase }
            phraseResults.append(PhraseResult(index: phrase, reference: Phrases.all[phrase], run: run))
            phraseResults.sort { $0.index < $1.index }
            if phrase == phraseIndex, phraseIndex < Phrases.all.count - 1 { phraseIndex += 1 }
        }
        print("SPIKE_RESULT \(Report.runLine(run))")
        try? FileManager.default.removeItem(at: clip.fileURL)
    }

    // MARK: Test hook

    /// For simulator runs: SPIKE_AUDIO_FILE names an audio file (absolute, or relative to
    /// Documents). The app downloads and loads the model, transcribes the file, prints
    /// the report to stdout, and keeps running.
    func runAutomaticTestIfRequested() async {
        guard let name = ProcessInfo.processInfo.environment["SPIKE_AUDIO_FILE"], !name.isEmpty else { return }
        let docs = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
        let url = name.hasPrefix("/") ? URL(fileURLWithPath: name) : docs.appendingPathComponent(name)
        log("Automatic test with \(url.lastPathComponent)")
        await downloadAndLoad()
        await prepareApple()
        do {
            let clip = try Clip.load(contentsOf: url)
            let phrase = ProcessInfo.processInfo.environment["SPIKE_PHRASE"].flatMap(Int.init).map { $0 - 1 }
            await transcribe(clip, label: url.lastPathComponent, phrase: phrase)
        } catch {
            log("Automatic test failed: \(error.localizedDescription)")
        }
        print("SPIKE_REPORT_BEGIN\n\(report())\nSPIKE_REPORT_END")
    }

    // MARK: Log and report

    func log(_ text: String) {
        events.append(Event(time: Date(), text: text))
        print("SPIKE_EVENT \(text)")
    }

    func report() -> String {
        Report.markdown(model: self)
    }

    func copyReport() {
        UIPasteboard.general.string = report()
        log("Report copied to the clipboard")
    }

    static var deviceLine: String {
        var info = utsname()
        uname(&info)
        let machine = withUnsafeBytes(of: &info.machine) { raw in
            String(decoding: raw.prefix { $0 != 0 }, as: UTF8.self)
        }
        let device = UIDevice.current
        let ram = Double(ProcessInfo.processInfo.physicalMemory) / 1_073_741_824
        return String(format: "%@ (%@), %@ %@, %.0f GB RAM", device.model, machine,
                      device.systemName, device.systemVersion, ram)
    }
}
