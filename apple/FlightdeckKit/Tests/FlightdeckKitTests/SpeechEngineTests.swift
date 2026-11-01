import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// A speech engine with scripted answers.
@MainActor
final class FakeEngine: SpeechEngine {
    let kind: SpeechEngineKind
    var prepareResult: Result<SpeechEngineStatus, Error> = .success(.ready)
    var results: [Result<String, Error>] = []
    private(set) var prepares = 0
    private(set) var transcribes = 0
    private(set) var closes = 0

    init(_ kind: SpeechEngineKind, _ results: [Result<String, Error>] = []) {
        self.kind = kind
        self.results = results
    }

    func prepare() async throws -> SpeechEngineStatus {
        prepares += 1
        return try prepareResult.get()
    }

    func transcribe(_ clip: SpeechClip, live: String) async throws -> String {
        transcribes += 1
        guard !results.isEmpty else { return live }
        return try (results.count > 1 ? results.removeFirst() : results[0]).get()
    }

    func close() async { closes += 1 }
}

/// A Parakeet runtime that counts loads and unloads.
final class FakeRuntime: ParakeetRuntime, @unchecked Sendable {
    private let lock = NSLock()
    private var _downloaded: Bool
    private var _loaded = false
    private(set) var loadCount = 0
    private(set) var unloadCount = 0
    var loadDelay: Duration = .zero
    var text = "on device text"

    init(downloaded: Bool = true) { _downloaded = downloaded }

    var loaded: Bool { lock.withLock { _loaded } }

    func isDownloaded() async -> Bool { lock.withLock { _downloaded } }
    func download(progress: @escaping @Sendable (Double) -> Void) async throws {
        progress(0.5)
        progress(1)
        lock.withLock { _downloaded = true }
    }
    func removeDownload() async throws { lock.withLock { _downloaded = false } }
    func load() async throws {
        if loadDelay > .zero { try await Task.sleep(for: loadDelay) }
        lock.withLock { _loaded = true; loadCount += 1 }
    }
    func unload() async { lock.withLock { _loaded = false; unloadCount += 1 } }
    func transcribe(_ samples: [Float]) async throws -> String {
        guard loaded else { throw SpeechEngineError.unavailable("not loaded", sticky: false) }
        return text
    }
}

/// A `SpeechHab` with scripted answers.
final class FakeSpeechHab: SpeechHab, @unchecked Sendable {
    var speech = true
    var prepareResult: Result<SpeechPrepareResult, Error> = .success(SpeechPrepareResult(state: .ready, formats: [.opus, .pcm16]))
    var transcribeResult: Result<SpeechTranscribeResult, Error> = .success(SpeechTranscribeResult(text: "hab text"))
    private(set) var sent: [SpeechTranscribeParams] = []
    private(set) var prepares = 0

    func hasSpeech() async -> Bool { speech }
    func prepare() async throws -> SpeechPrepareResult {
        prepares += 1
        return try prepareResult.get()
    }
    func transcribe(_ p: SpeechTranscribeParams) async throws -> SpeechTranscribeResult {
        sent.append(p)
        return try transcribeResult.get()
    }
}

func rpcError(_ kind: ErrorKind, _ hint: String) -> RPCError {
    RPCError(code: HabAPI.errorCode, message: hint, data: ErrorData(hint: hint, kind: kind, seq: 0))
}

@MainActor
final class SpeechEngineTests: XCTestCase {
    let clip = SpeechClip(samples: Array(repeating: 0.1, count: 16_000))

    // MARK: - Fallback

    func testFirstEngineWins() async {
        let hab = FakeEngine(.hab, [.success("from hab")])
        let apple = FakeEngine(.apple)
        let e = SpeechEngines([hab, apple])
        let r = await e.transcribe(clip, live: "live words")
        XCTAssertEqual(r?.text, "from hab")
        XCTAssertEqual(r?.engine, .hab)
        XCTAssertEqual(apple.transcribes, 0)
        XCTAssertNil(e.lastFallback)
    }

    func testUnavailableMovesToTheNextEngine() async {
        let hab = FakeEngine(.hab, [.failure(SpeechEngineError.unavailable("downloading 40%", sticky: false)), .success("hab back")])
        let parakeet = FakeEngine(.parakeet, [.success("on device")])
        let e = SpeechEngines([hab, parakeet, FakeEngine(.apple)])
        let r = await e.transcribe(clip, live: "")
        XCTAssertEqual(r?.engine, .parakeet)
        XCTAssertEqual(r?.text, "on device")
        XCTAssertEqual(e.lastFallback, "Parakeet on the hab: downloading 40%")
        // Not sticky: the next turn tries the hab first again.
        let r2 = await e.transcribe(clip, live: "")
        XCTAssertEqual(r2?.engine, .hab)
        XCTAssertEqual(hab.transcribes, 2)
    }

    func testStickyRefusalSkipsUntilClose() async {
        let hab = FakeEngine(.hab)
        hab.prepareResult = .failure(SpeechEngineError.unavailable("this hab has no speech engine", sticky: true))
        let apple = FakeEngine(.apple)
        let e = SpeechEngines([hab, apple])
        await e.prepare()
        XCTAssertEqual(e.skipped[.hab], "this hab has no speech engine")
        XCTAssertEqual(e.first, .apple)
        let r = await e.transcribe(clip, live: "live words")
        XCTAssertEqual(r?.engine, .apple)
        XCTAssertEqual(r?.text, "live words")
        XCTAssertEqual(hab.transcribes, 0)
        await e.close()
        XCTAssertEqual(hab.closes, 1)
        XCTAssertEqual(apple.closes, 1)
        XCTAssertTrue(e.skipped.isEmpty)
    }

    func testAnyErrorFallsBackForThisTurn() async {
        let hab = FakeEngine(.hab, [.failure(ConnectionClosed(detail: "gone"))])
        let e = SpeechEngines([hab, FakeEngine(.apple)])
        let r = await e.transcribe(clip, live: "heard")
        XCTAssertEqual(r?.engine, .apple)
        XCTAssertNil(e.skipped[.hab])
    }

    func testEveryEngineFailing() async {
        let e = SpeechEngines([FakeEngine(.hab, [.failure(SpeechEngineError.unavailable("off", sticky: true))])])
        let r = await e.transcribe(clip, live: "")
        XCTAssertNil(r)
        XCTAssertEqual(e.lastFallback, "Parakeet on the hab: off")
        let none = await SpeechEngines([]).transcribe(clip, live: "x")
        XCTAssertNil(none)
    }

    // MARK: - The hab engine

    func testHabWithoutSpeechCapabilityIsSticky() async {
        let api = FakeSpeechHab()
        api.speech = false
        let engine = HabSpeechEngine(hab: api, session: "s1")
        do {
            _ = try await engine.prepare()
            XCTFail("prepare should refuse")
        } catch let SpeechEngineError.unavailable(_, sticky) {
            XCTAssertTrue(sticky)
        } catch {
            XCTFail("\(error)")
        }
        XCTAssertEqual(api.prepares, 0)
    }

    func testHabUnavailableOnPrepareIsSticky() async {
        let api = FakeSpeechHab()
        api.prepareResult = .failure(rpcError(.unavailable, "the speech engine is off on this host"))
        let e = SpeechEngines([HabSpeechEngine(hab: api, session: "s1"), FakeEngine(.apple)])
        await e.prepare()
        XCTAssertEqual(e.skipped[.hab], "the speech engine is off on this host")
    }

    func testHabDownloadingFallsBackThenUsesHabWhenReady() async {
        let api = FakeSpeechHab()
        api.prepareResult = .success(SpeechPrepareResult(state: .downloading, progress: 40, formats: [.pcm16]))
        let hab = HabSpeechEngine(hab: api, session: "s1", encode: { _ in Data() })
        let e = SpeechEngines([hab, FakeEngine(.apple)])
        await e.prepare()
        XCTAssertEqual(e.status[.hab], .preparing("downloading 40%"))
        var r = await e.transcribe(clip, live: "live")
        XCTAssertEqual(r?.engine, .apple)
        XCTAssertEqual(e.lastFallback, "Parakeet on the hab: downloading 40%")
        XCTAssertTrue(api.sent.isEmpty)
        api.prepareResult = .success(SpeechPrepareResult(state: .ready, formats: [.pcm16]))
        r = await e.transcribe(clip, live: "live")
        XCTAssertEqual(r?.engine, .hab)
        XCTAssertEqual(r?.text, "hab text")
    }

    func testHabUnavailableOnTranscribeFallsBack() async {
        let api = FakeSpeechHab()
        api.transcribeResult = .failure(rpcError(.unavailable, "downloading 10%"))
        let e = SpeechEngines([HabSpeechEngine(hab: api, session: "s1", encode: { _ in Data([1]) }), FakeEngine(.apple)])
        await e.prepare()
        let r = await e.transcribe(clip, live: "live")
        XCTAssertEqual(r?.engine, .apple)
        XCTAssertEqual(e.lastFallback, "Parakeet on the hab: downloading 10%")
        XCTAssertNil(e.skipped[.hab])
    }

    func testHabBodyPrefersOpusAndFallsBackToPCM() async throws {
        let api = FakeSpeechHab()
        let opus = HabSpeechEngine(hab: api, session: "s1", encode: { _ in Data([0x4f, 0x67, 0x67, 0x53]) })
        _ = try await opus.prepare()
        var b = try opus.body(clip)
        XCTAssertEqual(b.format, .opus)
        XCTAssertNil(b.sampleRate)
        XCTAssertEqual(b.session, "s1")
        XCTAssertEqual(b.language, "en")

        // The encoder fails: 16-bit PCM at 16 kHz.
        let broken = HabSpeechEngine(hab: api, session: "s1", encode: { _ in throw OpusEncoder.Failure(description: "no encoder") })
        _ = try await broken.prepare()
        b = try broken.body(clip)
        XCTAssertEqual(b.format, .pcm16)
        XCTAssertEqual(b.sampleRate, 16_000)
        XCTAssertEqual(Data(base64Encoded: b.audio)?.count, clip.samples.count * 2)

        // The hab takes only PCM.
        api.prepareResult = .success(SpeechPrepareResult(state: .ready, formats: [.pcm16]))
        let pcmOnly = HabSpeechEngine(hab: api, session: nil, encode: { _ in Data([1]) })
        _ = try await pcmOnly.prepare()
        XCTAssertEqual(try pcmOnly.body(clip).format, .pcm16)
    }

    func testHabRefusesAClipOverTheLimits() async throws {
        let api = FakeSpeechHab()
        let hab = HabSpeechEngine(hab: api, session: "s1", encode: { _ in Data() })
        _ = try await hab.prepare()
        let long = SpeechClip(samples: Array(repeating: 0, count: 121 * 16_000))
        do {
            _ = try await hab.transcribe(long, live: "")
            XCTFail("a 121 s clip should be refused")
        } catch let SpeechEngineError.unavailable(_, sticky) {
            XCTAssertFalse(sticky)
        }
        XCTAssertTrue(api.sent.isEmpty)
    }

    func testPCM16IsLittleEndianAndClamped() {
        let c = SpeechClip(samples: [0, 1, -1, 2])
        XCTAssertEqual([UInt8](c.pcm16), [0, 0, 0xff, 0x7f, 0x01, 0x80, 0xff, 0x7f])
    }

    func testAppleEngineUsesTheLiveWords() async throws {
        let apple = AppleSpeechEngine(supported: true)
        let heard = try await apple.transcribe(clip, live: " heard this ")
        XCTAssertEqual(heard, "heard this")
        do {
            _ = try await apple.transcribe(clip, live: "  ")
            XCTFail("no words should be refused")
        } catch SpeechEngineError.unavailable(_, let sticky) {
            XCTAssertFalse(sticky)
        }
        do {
            _ = try await AppleSpeechEngine(supported: false).prepare()
            XCTFail("an old system should refuse")
        } catch SpeechEngineError.unavailable(_, let sticky) {
            XCTAssertTrue(sticky)
        }
    }

    // MARK: - The hab engine against the fake hab

    func testHabEngineOverTheFakeHab() async throws {
        let fake = await sampleHab()
        await fake.setCapability(speechCapability, true)
        await fake.setTranscripts(["run the link tests"])
        let fleet = Fleet()
        let conn = HabConnection(id: "h1", transport: fake.transport(), timing: fastTiming())
        fleet.add(conn, label: "h1")
        await fleet.setActive(true)
        await eventually("online") { await conn.state.status == .online }
        let e = SpeechEngines([HabSpeechEngine(hab: SpeechAPI(connection: conn), session: "s1"), AppleSpeechEngine(supported: true)])
        await e.prepare()
        XCTAssertEqual(e.status[.hab], .ready)
        let r = await e.transcribe(clip, live: "run the link test")
        XCTAssertEqual(r?.engine, .hab)
        XCTAssertEqual(r?.text, "run the link tests")
        let sent = await fake.transcribes
        XCTAssertEqual(sent.count, 1)
        XCTAssertEqual(sent.first?.session, "s1")

        // A hab whose model is downloading answers unavailable: the turn goes to Apple.
        await fake.setSpeechPrepare(SpeechPrepareResult(state: .downloading, progress: 40))
        await fake.failNextTranscribe(kind: .unavailable, hint: "downloading 40%")
        let r2 = await e.transcribe(clip, live: "fallback words")
        XCTAssertEqual(r2?.engine, .apple)
        XCTAssertEqual(r2?.text, "fallback words")
        await fleet.setActive(false)
    }

    func testHabEngineWithoutTheCapabilityOverTheFakeHab() async {
        let fake = await sampleHab()
        let fleet = Fleet()
        let conn = HabConnection(id: "h1", transport: fake.transport(), timing: fastTiming())
        fleet.add(conn, label: "h1")
        await fleet.setActive(true)
        await eventually("online") { await conn.state.status == .online }
        let e = SpeechEngines([HabSpeechEngine(hab: SpeechAPI(connection: conn), session: "s1"), AppleSpeechEngine(supported: true)])
        await e.prepare()
        XCTAssertNotNil(e.skipped[.hab])
        let r = await e.transcribe(clip, live: "words")
        XCTAssertEqual(r?.engine, .apple)
        let n = await fake.speechPrepares
        XCTAssertEqual(n, 0)
        await fleet.setActive(false)
    }

    // MARK: - Parakeet load and unload rules

    func testParakeetLoadsWhileHeldAndUnloadsOnLastRelease() async throws {
        let rt = FakeRuntime()
        let host = ParakeetHost(runtime: rt, offered: true)
        try await host.acquire("voice")
        XCTAssertTrue(host.isLoaded)
        try await host.acquire("dictation")
        XCTAssertEqual(rt.loadCount, 1, "one load for two holders")
        await host.release("voice")
        XCTAssertTrue(host.isLoaded, "dictation still holds it")
        await host.release("dictation")
        XCTAssertFalse(host.isLoaded)
        XCTAssertFalse(rt.loaded)
        XCTAssertEqual(rt.unloadCount, 1)
    }

    func testParakeetUnloadsInTheBackgroundAndReloadsOnReturn() async throws {
        let rt = FakeRuntime()
        let host = ParakeetHost(runtime: rt, offered: true)
        let engine = ParakeetSpeechEngine(host: host, holder: "voice")
        _ = try await engine.prepare()
        await host.setForeground(false)
        XCTAssertFalse(rt.loaded)
        do {
            _ = try await engine.transcribe(clip, live: "")
            XCTFail("no transcription in the background")
        } catch SpeechEngineError.unavailable(_, let sticky) {
            XCTAssertFalse(sticky, "the background is not sticky")
        }
        await host.setForeground(true)
        XCTAssertTrue(rt.loaded)
        let text = try await engine.transcribe(clip, live: "")
        XCTAssertEqual(text, "on device text")
        XCTAssertEqual(rt.loadCount, 2)
        await engine.close()
        XCTAssertFalse(rt.loaded)
    }

    func testParakeetStaysUnloadedAfterAMemoryWarningUntilReleased() async throws {
        let rt = FakeRuntime()
        let host = ParakeetHost(runtime: rt, offered: true)
        let e = SpeechEngines([ParakeetSpeechEngine(host: host, holder: "voice"), FakeEngine(.apple)])
        await e.prepare()
        XCTAssertTrue(rt.loaded)
        await host.memoryWarning()
        XCTAssertFalse(rt.loaded)
        let r = await e.transcribe(clip, live: "apple words")
        XCTAssertEqual(r?.engine, .apple)
        XCTAssertNotNil(e.skipped[.parakeet])
        await host.setForeground(true)
        XCTAssertFalse(rt.loaded, "no reload after a memory warning")
        await e.close()
        // A new voice mode may load it again.
        try await host.acquire("voice")
        XCTAssertTrue(rt.loaded)
        await host.release("voice")
    }

    func testParakeetNotDownloadedOrNotOffered() async {
        let notDownloaded = ParakeetHost(runtime: FakeRuntime(downloaded: false), offered: true)
        do {
            try await notDownloaded.acquire("voice")
            XCTFail("needs the model")
        } catch SpeechEngineError.unavailable(_, let sticky) {
            XCTAssertTrue(sticky)
        } catch { XCTFail("\(error)") }
        let small = ParakeetHost(runtime: FakeRuntime(), offered: false)
        XCTAssertFalse(small.offered)
        let none = ParakeetHost(runtime: nil, offered: true)
        XCTAssertFalse(none.offered)
        XCTAssertTrue(ParakeetHost.deviceOffers(physicalMemory: 8 << 30))
        XCTAssertTrue(ParakeetHost.deviceOffers(physicalMemory: (6 << 30) - (300 << 20)))
        XCTAssertFalse(ParakeetHost.deviceOffers(physicalMemory: 4 << 30))
    }

    func testParakeetReleaseDuringLoadUnloads() async throws {
        let rt = FakeRuntime()
        rt.loadDelay = .milliseconds(100)
        let host = ParakeetHost(runtime: rt, offered: true)
        let t = Task { try await host.acquire("voice") }
        try await Task.sleep(for: .milliseconds(20))
        await host.release("voice")
        _ = try? await t.value
        XCTAssertFalse(rt.loaded, "a load that finishes after the release is undone")
        XCTAssertFalse(host.isLoaded)
    }

    func testParakeetDownloadFromSettings() async {
        let rt = FakeRuntime(downloaded: false)
        let host = ParakeetHost(runtime: rt, offered: true)
        await host.refresh()
        XCTAssertFalse(host.isDownloaded)
        await host.download()
        XCTAssertTrue(host.isDownloaded)
        XCTAssertNil(host.downloadProgress)
        await host.removeDownload()
        XCTAssertFalse(host.isDownloaded)
    }
}
