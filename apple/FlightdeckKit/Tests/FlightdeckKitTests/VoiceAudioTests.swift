import AVFoundation
import Foundation
import XCTest
@testable import FlightdeckKit

/// The Ogg Opus writer, the system Opus encoder, and turn detection.
final class VoiceAudioTests: XCTestCase {
    /// One parsed Ogg page.
    struct Page {
        var flags: UInt8
        var granule: Int64
        var sequence: UInt32
        var packets: [Data]
        var crcOK: Bool
    }

    /// A small Ogg reader: pages, their packets (whole packets only) and CRCs.
    func pages(_ d: Data) -> [Page] {
        var out: [Page] = []
        var i = d.startIndex
        func u32(_ at: Int) -> UInt32 { d[at..<at + 4].enumerated().reduce(0) { $0 | UInt32($1.element) << (8 * $1.offset) } }
        func i64(_ at: Int) -> Int64 { Int64(bitPattern: d[at..<at + 8].enumerated().reduce(UInt64(0)) { $0 | UInt64($1.element) << (8 * $1.offset) }) }
        while i < d.endIndex {
            XCTAssertEqual(d[i..<i + 4], Data("OggS".utf8))
            let n = Int(d[i + 26])
            let table = Array(d[(i + 27)..<(i + 27 + n)])
            var at = i + 27 + n
            var packets: [Data] = []
            var cur = Data()
            for l in table {
                cur.append(d[at..<at + Int(l)])
                at += Int(l)
                if l < 255 { packets.append(cur); cur = Data() }
            }
            var copy = Data(d[i..<at])
            let stored = u32(i + 22)
            copy.replaceSubrange(22..<26, with: [0, 0, 0, 0])
            out.append(Page(flags: d[i + 5], granule: i64(i + 6), sequence: u32(i + 18), packets: packets, crcOK: OggOpusWriter.crc32(copy) == stored))
            i = at
        }
        return out
    }

    #if os(iOS)
    /// Voice mode and dictation use a category the silent switch does not
    /// mute, keep the microphone, and duck other audio (app.md 13.7).
    @MainActor
    func testAudioSessionPlaysWithTheSilentSwitchOn() {
        for speaks in [true, false] {
            let c = VoiceAudio.sessionConfig(speaks: speaks)
            XCTAssertEqual(c.category, .playAndRecord, "the silent switch never mutes playAndRecord")
            XCTAssertTrue(c.options.contains(.duckOthers))
            XCTAssertTrue(c.options.contains(.defaultToSpeaker))
        }
        XCTAssertEqual(VoiceAudio.sessionConfig(speaks: true).mode, .voiceChat, "echo cancellation for barge-in")
    }

    /// Speech moves to the loudspeaker only when the earpiece is the sole
    /// output; a headset, AirPods or a car keep their route.
    @MainActor
    func testSpeakerOverrideOnlyForTheEarpiece() {
        XCTAssertTrue(VoiceAudio.needsSpeaker(outputs: [.builtInReceiver]))
        XCTAssertFalse(VoiceAudio.needsSpeaker(outputs: [.builtInSpeaker]))
        XCTAssertFalse(VoiceAudio.needsSpeaker(outputs: [.headphones]))
        XCTAssertFalse(VoiceAudio.needsSpeaker(outputs: [.bluetoothHFP]))
        XCTAssertFalse(VoiceAudio.needsSpeaker(outputs: [.bluetoothA2DP]))
        XCTAssertFalse(VoiceAudio.needsSpeaker(outputs: [.carAudio]))
        XCTAssertFalse(VoiceAudio.needsSpeaker(outputs: []))
    }
    #endif

    func testCRCMatchesTheOggPolynomial() {
        // CRC-32/MPEG-2 without the final xor and initial ones: the Ogg variant.
        XCTAssertEqual(OggOpusWriter.crc32(Data("123456789".utf8)), 0x89a1_897f)
        XCTAssertEqual(OggOpusWriter.crc32(Data()), 0)
    }

    func testFileStructure() {
        let packets = (0..<300).map { i -> Data in
            var d = Data(repeating: UInt8(i % 256), count: i % 7 == 0 ? 300 : 60)
            d[0] = 9 << 3 // SILK, 20 ms
            return d
        }
        let file = OggOpusWriter.file(packets: packets, preSkip: 312, inputSampleRate: 16_000, sampleCount: 300 * 320 - 100)
        let ps = pages(file)
        XCTAssertGreaterThanOrEqual(ps.count, 4)
        XCTAssertTrue(ps.allSatisfy(\.crcOK))
        XCTAssertEqual(ps.map(\.sequence), (0..<UInt32(ps.count)).map { $0 })
        // The ID header: BOS, OpusHead, version 1, mono, pre-skip, input rate.
        XCTAssertEqual(ps[0].flags, 0x02)
        let head = ps[0].packets[0]
        XCTAssertEqual(head.prefix(8), Data("OpusHead".utf8))
        XCTAssertEqual(Array(head[8...11]), [1, 1, 0x38, 0x01])
        XCTAssertEqual(head.count, 19)
        XCTAssertEqual(ps[1].packets[0].prefix(8), Data("OpusTags".utf8))
        XCTAssertEqual(ps[1].granule, 0)
        // Every packet comes back whole and in order; the last page ends the stream.
        let audio = ps.dropFirst(2).flatMap(\.packets)
        XCTAssertEqual(audio, packets)
        XCTAssertEqual(ps.last?.flags, 0x04)
        // Granules rise and the last one marks the real end (48 kHz), before the padding.
        let granules = ps.dropFirst(2).map(\.granule)
        XCTAssertEqual(granules, granules.sorted())
        XCTAssertEqual(ps.last?.granule, 312 + Int64(300 * 320 - 100) * 3)
    }

    func testPacketDurationFromTheTOCByte() {
        XCTAssertEqual(OggOpusWriter.samples48k(Data([9 << 3])), 960, "SILK 20 ms")
        XCTAssertEqual(OggOpusWriter.samples48k(Data([31 << 3])), 960, "CELT 20 ms")
        XCTAssertEqual(OggOpusWriter.samples48k(Data([16 << 3])), 120, "CELT 2.5 ms")
        XCTAssertEqual(OggOpusWriter.samples48k(Data([(11 << 3) | 1])), 5760, "two SILK 60 ms frames")
        XCTAssertEqual(OggOpusWriter.samples48k(Data([(9 << 3) | 3, 3])), 2880, "code 3, three frames")
        XCTAssertEqual(OggOpusWriter.samples48k(Data()), 0)
    }

    func testPacketOfExactly255BytesGetsAZeroLacing() {
        let page = OggOpusWriter.page(packets: [Data(repeating: 1, count: 255)], granule: 0, serial: 1, sequence: 0, flags: 0)
        XCTAssertEqual(page[26], 2)
        XCTAssertEqual(Array(page[27...28]), [255, 0])
        XCTAssertEqual(pages(page).first?.packets.first?.count, 255)
    }

    func testSystemOpusEncoderWritesOggOpus() throws {
        // 1 s of a 440 Hz tone.
        let step: Double = 2 * Double.pi * 440 / 16_000
        let samples: [Float] = (0..<16_000).map { (i: Int) -> Float in Float(0.3 * sin(step * Double(i))) }
        let data: Data
        do {
            data = try OpusEncoder.oggOpus(SpeechClip(samples: samples))
        } catch {
            throw XCTSkip("no Opus encoder on this system: \(error)")
        }
        let ps = pages(data)
        XCTAssertTrue(ps.allSatisfy(\.crcOK))
        let audio = ps.dropFirst(2).flatMap(\.packets)
        XCTAssertGreaterThanOrEqual(audio.count, 50, "20 ms packets for 1 s")
        // About 24 kbit/s: 3 KB per second, with room for headers and the encoder's choices.
        XCTAssertLessThan(data.count, 8_000, "\(data.count) bytes for 1 s")
        XCTAssertGreaterThan(data.count, 500)
    }

    // MARK: - Turn detection

    func testSilenceAfterSpeechEndsTheTurn() {
        var d = TurnDetector(silence: 1.2, threshold: -40)
        XCTAssertNil(d.level(-60, at: 0))
        XCTAssertNil(d.level(-20, at: 0.1))
        XCTAssertEqual(d.level(-20, at: 0.3), .speechStarted)
        XCTAssertNil(d.level(-20, at: 1.0))
        XCTAssertNil(d.level(-60, at: 1.5))
        XCTAssertNil(d.tick(at: 2.1))
        XCTAssertEqual(d.tick(at: 2.2), .turnEnded)
        XCTAssertFalse(d.inSpeech)
        XCTAssertNil(d.tick(at: 9))
    }

    func testAClickDoesNotStartATurn() {
        var d = TurnDetector(silence: 1.2, threshold: -40)
        XCTAssertNil(d.level(-10, at: 0))
        XCTAssertNil(d.level(-60, at: 0.05))
        XCTAssertNil(d.level(-10, at: 0.1))
        XCTAssertNil(d.level(-60, at: 0.2))
        XCTAssertFalse(d.inSpeech)
    }

    func testWordsCountAsSpeechButNotWhileSpeaking() {
        var d = TurnDetector()
        XCTAssertNil(d.words(at: 0, speaking: true))
        XCTAssertEqual(d.words(at: 0.1), .speechStarted)
        XCTAssertNil(d.words(at: 1.0))
        XCTAssertNil(d.tick(at: 2.1))
        XCTAssertEqual(d.tick(at: 2.25), .turnEnded)
    }

    func testBargeInNeedsMoreLevelWhileSpeaking() {
        var d = TurnDetector(silence: 1.2, threshold: -40)
        // The echo canceller leaves the app's voice at -35 dB: not a barge-in.
        XCTAssertNil(d.level(-35, at: 0, speaking: true))
        XCTAssertNil(d.level(-35, at: 0.5, speaking: true))
        XCTAssertFalse(d.inSpeech)
        // The owner talks over it at -20 dB.
        XCTAssertNil(d.level(-20, at: 1.0, speaking: true))
        XCTAssertEqual(d.level(-20, at: 1.2, speaking: true), .speechStarted)
    }

    func testLevelOfSamples() {
        let silent = [Float](repeating: 0, count: 100)
        XCTAssertEqual(silent.withUnsafeBufferPointer(audioLevel), -160)
        let full = [Float](repeating: 1, count: 100)
        XCTAssertEqual(full.withUnsafeBufferPointer(audioLevel), 0, accuracy: 0.01)
        let tenth = [Float](repeating: 0.1, count: 100)
        XCTAssertEqual(tenth.withUnsafeBufferPointer(audioLevel), -20, accuracy: 0.01)
    }

    func testATurnStopsAt120SecondsAndSaysSo() async throws {
        let (stream, sink) = AsyncStream.makeStream(of: VoiceInputEvent.self)
        let tap = TapState(sink: sink)
        let fmt = AVAudioFormat(standardFormatWithSampleRate: 16_000, channels: 1)!
        tap.reset(input: fmt, live: nil)
        tap.beginTurn()
        let buf = AVAudioPCMBuffer(pcmFormat: fmt, frameCapacity: 16_000)!
        buf.frameLength = 16_000
        for i in 0..<16_000 { buf.floatChannelData![0][i] = 0.1 }
        for _ in 0..<125 { tap.take(buf) }
        sink.finish()
        var full = 0
        for await e in stream where e == .turnFull { full += 1 }
        XCTAssertEqual(full, 1)
        let (clip, _) = tap.endTurn()
        XCTAssertEqual(clip.duration, 120, accuracy: 0.5, "the start is kept, the rest is not taken")
    }

    func testLiveTextSplitsTurnsAndSurvivesRevisions() {
        var t = LiveText()
        // Earlier speech, finished before the turn.
        t.update(finished: "hello there.", volatile: "", at: 0)
        t.end()
        // The turn starts from words still in progress: they belong to it.
        t.update(finished: "hello there.", volatile: "run the", at: 5)
        t.begin(at: 5.1)
        XCTAssertEqual(t.turn, "run the")
        // The recognizer revises the result in progress: never the history.
        t.update(finished: "hello there.", volatile: "rerun the tests", at: 5.5)
        XCTAssertEqual(t.turn, "rerun the tests")
        t.update(finished: "hello there. rerun the tests.", volatile: "", at: 6)
        XCTAssertEqual(t.turn, "rerun the tests.")
        t.end()
        XCTAssertEqual(t.turn, "")
    }

    func testTheEndedTurnsPhraseInProgressStaysWithIt() {
        var t = LiveText()
        t.update(finished: "", volatile: "deploy it", at: 0)
        t.begin(at: 0.1)
        XCTAssertEqual(t.turn, "deploy it")
        t.end()
        // The phrase is revised, then finishes, after the turn ended: no new turn text.
        t.update(finished: "", volatile: "deploy it now", at: 0.5)
        XCTAssertEqual(t.turn, "")
        t.update(finished: "deploy it now.", volatile: "", at: 1.5)
        XCTAssertEqual(t.turn, "")
        t.begin(at: 1.7)
        XCTAssertEqual(t.turn, "", "no carry of the ended turn's phrase")
        t.update(finished: "deploy it now.", volatile: "and check", at: 2)
        XCTAssertEqual(t.turn, "and check")
    }

    func testTheEndedTurnsPhraseRaisesNoWordsEvent() async {
        let (stream, sink) = AsyncStream.makeStream(of: VoiceInputEvent.self)
        let tap = TapState(sink: sink)
        tap.liveChanged(finished: "", volatile: "deploy it")
        tap.beginTurn()
        _ = tap.endTurn()
        tap.liveChanged(finished: "deploy it.", volatile: "")
        sink.finish()
        var after = 0
        var all: [VoiceInputEvent] = []
        for await e in stream { all.append(e) }
        if let i = all.lastIndex(where: { if case .words = $0 { return true }; return false }) { after = i }
        XCTAssertEqual(all.filter { if case .words = $0 { return true }; return false }.count, 1, "only the turn's own words: \(all)")
        XCTAssertEqual(after, 0)
    }

    func testLiveTextKeepsAResultThatFinishedJustBeforeTheTurn() {
        var t = LiveText()
        t.update(finished: "", volatile: "stop", at: 0)
        t.update(finished: "stop.", volatile: "", at: 0.2)
        t.begin(at: 0.4)
        XCTAssertEqual(t.turn, "stop.", "the words that started the turn")
        t.end()
        // A result that finished long before a turn stays out.
        t.update(finished: "stop. noise.", volatile: "", at: 3)
        t.begin(at: 10)
        XCTAssertEqual(t.turn, "")
    }
}
