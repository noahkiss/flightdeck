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

    func testCRCMatchesTheOggPolynomial() {
        // CRC-32/MPEG-2 without the final xor and initial ones: the Ogg variant.
        XCTAssertEqual(OggOpusWriter.crc32(Data("123456789".utf8)), 0x89a1_897f)
        XCTAssertEqual(OggOpusWriter.crc32(Data()), 0)
    }

    func testFileStructure() {
        let packets = (0..<300).map { Data(repeating: UInt8($0 % 256), count: $0 % 7 == 0 ? 300 : 60) }
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

    func testPacketOfExactly255BytesGetsAZeroLacing() {
        let page = OggOpusWriter.page(packets: [Data(repeating: 1, count: 255)], granule: 0, serial: 1, sequence: 0, flags: 0)
        XCTAssertEqual(page[26], 2)
        XCTAssertEqual(Array(page[27...28]), [255, 0])
        XCTAssertEqual(pages(page).first?.packets.first?.count, 255)
    }

    func testSystemOpusEncoderWritesOggOpus() throws {
        // 1 s of a 440 Hz tone.
        let samples = (0..<16_000).map { Float(0.3 * sin(2 * .pi * 440 * Double($0) / 16_000)) }
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

    func testTurnTextCutsWhatCameBefore() {
        XCTAssertEqual(TapState.turnText(all: "hello there run the tests", base: "hello there"), "run the tests")
        XCTAssertEqual(TapState.turnText(all: "rewritten text", base: "hello there"), "rewritten text")
        XCTAssertEqual(TapState.turnText(all: " a ", base: ""), "a")
    }
}
