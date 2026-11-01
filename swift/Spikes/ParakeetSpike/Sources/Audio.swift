import AVFoundation
import Foundation

/// A recorded or loaded clip: 16 kHz mono Float32 samples, plus the same audio as a WAV
/// file (Apple's analyzer reads a file; Parakeet takes the samples).
struct Clip {
    static let sampleRate: Double = 16_000

    let samples: [Float]
    let fileURL: URL
    var duration: Double { Double(samples.count) / Clip.sampleRate }

    static var targetFormat: AVAudioFormat {
        AVAudioFormat(commonFormat: .pcmFormatFloat32, sampleRate: sampleRate, channels: 1, interleaved: false)!
    }

    /// Writes samples to a 16-bit 16 kHz mono WAV in the temp folder.
    static func make(samples: [Float]) throws -> Clip {
        let url = FileManager.default.temporaryDirectory
            .appendingPathComponent("clip-\(UUID().uuidString).wav")
        let settings: [String: Any] = [
            AVFormatIDKey: kAudioFormatLinearPCM,
            AVSampleRateKey: sampleRate,
            AVNumberOfChannelsKey: 1,
            AVLinearPCMBitDepthKey: 16,
            AVLinearPCMIsFloatKey: false,
            AVLinearPCMIsBigEndianKey: false,
        ]
        let file = try AVAudioFile(forWriting: url, settings: settings,
                                   commonFormat: .pcmFormatFloat32, interleaved: false)
        let buffer = AVAudioPCMBuffer(pcmFormat: targetFormat, frameCapacity: AVAudioFrameCount(max(samples.count, 1)))!
        buffer.frameLength = AVAudioFrameCount(samples.count)
        samples.withUnsafeBufferPointer { src in
            buffer.floatChannelData![0].update(from: src.baseAddress!, count: samples.count)
        }
        try file.write(from: buffer)
        return Clip(samples: samples, fileURL: url)
    }

    /// Loads any audio file AVFoundation can read and converts it to 16 kHz mono.
    static func load(contentsOf url: URL) throws -> Clip {
        let file = try AVAudioFile(forReading: url)
        let format = file.processingFormat
        guard let input = AVAudioPCMBuffer(pcmFormat: format, frameCapacity: AVAudioFrameCount(file.length)) else {
            throw SpikeError("Could not allocate a buffer for \(url.lastPathComponent)")
        }
        try file.read(into: input)
        let converter = Resampler(from: format)
        return try make(samples: converter.convert(input, endOfStream: true))
    }
}

/// Converts PCM buffers of one format to 16 kHz mono Float32 samples.
final class Resampler {
    private let converter: AVAudioConverter
    private let ratio: Double

    init(from format: AVAudioFormat) {
        converter = AVAudioConverter(from: format, to: Clip.targetFormat)!
        ratio = Clip.sampleRate / format.sampleRate
    }

    /// Converts one buffer. Pass endOfStream for the last (or only) buffer so the
    /// converter flushes its tail.
    func convert(_ buffer: AVAudioPCMBuffer, endOfStream: Bool = false) -> [Float] {
        let capacity = AVAudioFrameCount(Double(buffer.frameLength) * ratio) + 1024
        guard let output = AVAudioPCMBuffer(pcmFormat: Clip.targetFormat, frameCapacity: capacity) else { return [] }
        var supplied = false
        var error: NSError?
        converter.convert(to: output, error: &error) { _, status in
            if supplied {
                status.pointee = endOfStream ? .endOfStream : .noDataNow
                return nil
            }
            supplied = true
            status.pointee = .haveData
            return buffer
        }
        guard error == nil, let data = output.floatChannelData else { return [] }
        return Array(UnsafeBufferPointer(start: data[0], count: Int(output.frameLength)))
    }
}

/// Push-to-talk recorder: taps the microphone and keeps 16 kHz mono samples in memory.
final class Recorder {
    private let engine = AVAudioEngine()
    private var samples: [Float] = []
    private let lock = NSLock()
    private(set) var isRecording = false

    static func requestPermission() async -> Bool {
        await AVAudioApplication.requestRecordPermission()
    }

    func start() throws {
        guard !isRecording else { return }
        let session = AVAudioSession.sharedInstance()
        try session.setCategory(.playAndRecord, mode: .default, options: [.defaultToSpeaker])
        try session.setActive(true)

        lock.withLock { samples.removeAll(keepingCapacity: true) }
        let input = engine.inputNode
        let format = input.outputFormat(forBus: 0)
        guard format.sampleRate > 0, format.channelCount > 0 else {
            throw SpikeError("No microphone input available")
        }
        let resampler = Resampler(from: format)
        input.installTap(onBus: 0, bufferSize: 4096, format: format) { [weak self] buffer, _ in
            let converted = resampler.convert(buffer)
            guard let self else { return }
            self.lock.withLock { self.samples.append(contentsOf: converted) }
        }
        engine.prepare()
        try engine.start()
        isRecording = true
    }

    func stop() throws -> Clip {
        guard isRecording else { throw SpikeError("Not recording") }
        engine.inputNode.removeTap(onBus: 0)
        engine.stop()
        isRecording = false
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
        let captured = lock.withLock { samples }
        return try Clip.make(samples: captured)
    }
}

struct SpikeError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}
