import Foundation
import os
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Collects what a pane stream applies and rebuilds the screen's bytes: the
/// last snapshot, then every byte after it.
actor Applied {
    private(set) var events: [PaneStreamEvent] = []
    private(set) var screen = Data()
    private(set) var snapshots = 0
    private(set) var welcomes = 0
    private(set) var ended: PaneStreamEnd?
    /// Where the next bytes frame must start, or later after a removed query.
    private(set) var next: UInt64?
    private(set) var overlaps = 0

    func apply(_ e: PaneStreamEvent) {
        events.append(e)
        switch e {
        case .welcome: welcomes += 1
        case .snapshot(let off, _, let d):
            screen = d
            snapshots += 1
            next = off
        case .bytes(let off, let d):
            if let n = next, off < n { overlaps += 1 }
            screen.append(d)
            next = off + UInt64(d.count)
        case .ended(let r): ended = r
        default: break
        }
    }

    var text: String { String(decoding: screen, as: UTF8.self) }

    var resizes: [ResizeFrame] {
        events.compactMap { if case .resize(let r) = $0 { return r } else { return nil } }
    }
}

final class PaneStreamTests: XCTestCase {
    private func online(_ hab: FakeHab) async -> HabConnection {
        let conn = HabConnection(id: "h", transport: hab.transport(), timing: fastTiming(), clientLabel: "test")
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        return conn
    }

    private func fastStream(_ pane: String, _ conn: HabConnection) -> PaneStream {
        var t = PaneStreamTiming()
        t.minBackoff = .milliseconds(10)
        t.maxBackoff = .milliseconds(40)
        return PaneStream(pane: pane, connection: conn, timing: t)
    }

    /// A scripted byte stream with four data drops mid-flow: the client ends
    /// with exactly the pane's output, one snapshot, no overlap, no trim.
    func testResumeAfterDropsLosesAndRepeatsNothing() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        await hab.terminals.write("%1", Data("hello\r\n".utf8))
        let conn = await online(hab)
        let stream = fastStream("%1", conn)
        let got = Applied()
        await stream.start { await got.apply($0) }
        await eventually("snapshot") { await got.snapshots == 1 }

        var rng = SystemRandomNumberGenerator()
        for i in 0..<240 {
            let n = Int.random(in: 1...3000, using: &rng)
            let line = String(format: "%05d ", i) + String(repeating: "abcdefghij", count: n / 10 + 1)
            await hab.terminals.write("%1", Data((line + "\r\n").utf8))
            if i % 60 == 30 {
                // Wait until the last drop was resumed, then drop again while output flows.
                let open = await stream.connections
                await eventually("reconnected") { await hab.terminals.dataConnectionCount == 1 }
                await hab.terminals.dropDataConnections()
                for j in 0..<3 {
                    await hab.terminals.write("%1", Data("while away \(i) \(j)\r\n".utf8))
                }
                await eventually("next connection") { await stream.connections == open + 1 }
            }
            if i % 7 == 0 { await Task.yield() }
        }
        let want = await hab.terminals.output("%1")
        await eventually("caught up", timeout: 10) { await got.screen.count == want.count }
        let screen = await got.screen
        XCTAssertEqual(screen, want)
        let snaps = await got.snapshots
        let overlaps = await got.overlaps
        let trimmed = await stream.trimmed
        let conns = await stream.connections
        XCTAssertEqual(snaps, 1, "a resume must not fall back to a snapshot")
        XCTAssertEqual(overlaps, 0)
        XCTAssertEqual(trimmed, 0)
        XCTAssertEqual(conns, 5, "four drops, five data connections")
        // Every hello after the first carried the held offset.
        let hellos = await stream.helloOffsets
        XCTAssertNil(hellos.first ?? nil)
        XCTAssertTrue(hellos.dropFirst().allSatisfy { $0 != nil })
        let held = await stream.offset
        let end = await hab.terminals.end("%1")
        XCTAssertEqual(held, end)
        await stream.close()
        await eventually("ended") { await got.ended == .closed }
    }

    /// Removed terminal queries are skips between frames; the held offset
    /// still ends at the pane's end, and a resume after them repeats nothing.
    func testSkipsBetweenFramesKeepExactOffsets() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let conn = await online(hab)
        let stream = fastStream("%1", conn)
        let got = Applied()
        await stream.start { await got.apply($0) }
        await eventually("snapshot") { await got.snapshots == 1 }
        await hab.terminals.write("%1", Data("a".utf8))
        await hab.terminals.skip("%1", 4)
        await hab.terminals.write("%1", Data("b".utf8))
        await eventually("bytes") { await got.screen == Data("ab".utf8) }
        await hab.terminals.dropDataConnections()
        await hab.terminals.write("%1", Data("c".utf8))
        await eventually("resumed") { await got.screen == Data("abc".utf8) }
        let held = await stream.offset
        XCTAssertEqual(held, 7)
        let trimmed = await stream.trimmed
        XCTAssertEqual(trimmed, 0)
        await stream.close()
    }

    /// A resume from an offset the hab no longer keeps starts with a snapshot.
    func testSnapshotWhenHistoryIsGone() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let conn = await online(hab)
        let stream = fastStream("%1", conn)
        let got = Applied()
        await stream.start { await got.apply($0) }
        await hab.terminals.write("%1", Data("one ".utf8))
        await eventually("bytes") { await got.screen == Data("one ".utf8) }
        await conn.stop()
        await hab.terminals.dropDataConnections()
        await hab.terminals.write("%1", Data("two ".utf8))
        await hab.terminals.forgetHistory("%1")
        await hab.terminals.write("%1", Data("three".utf8))
        await conn.start()
        await eventually("second snapshot") { await got.snapshots == 2 }
        await eventually("screen") { await got.screen == Data("one two three".utf8) }
        await stream.close()
    }

    func testExitEndsTheStream() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let conn = await online(hab)
        let stream = fastStream("%1", conn)
        let got = Applied()
        await stream.start { await got.apply($0) }
        await eventually("snapshot") { await got.snapshots == 1 }
        await hab.terminals.write("%1", Data("bye".utf8))
        await hab.terminals.exit("%1", code: 3)
        await eventually("ended") { await got.ended == .exited }
        let events = await got.events
        XCTAssertTrue(events.contains(.exited(ExitedFrame(code: 3))))
        let screen = await got.screen
        XCTAssertEqual(screen, Data("bye".utf8))
    }

    func testUnknownPaneIsGone() async throws {
        let hab = FakeHab(hostID: "box")
        let conn = await online(hab)
        let stream = fastStream("%9", conn)
        let got = Applied()
        await stream.start { await got.apply($0) }
        await eventually("ended") { await got.ended == .gone }
    }

    func testCreditIsReturned() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let conn = await online(hab)
        let stream = fastStream("%1", conn)
        let got = Applied()
        await stream.start { await got.apply($0) }
        await eventually("snapshot") { await got.snapshots == 1 }
        await hab.terminals.write("%1", Data(repeating: 0x41, count: 100 << 10))
        await eventually("credit") { await hab.terminals.creditReturned >= 96 << 10 }
        // The fake holds output past the 256 KiB window until credit returns,
        // as the hab does: 1 MiB more arrives whole, and the window was hit.
        // One write queues every frame at once, so the window is reached.
        await hab.terminals.write("%1", Data(repeating: 0x42, count: 1 << 20))
        let want = (100 << 10) + (1 << 20)
        await eventually("all output", timeout: 10) { await got.screen.count == want }
        let waits = await hab.terminals.creditWaits
        XCTAssertGreaterThan(waits, 0, "the window was reached at least once")
        await stream.close()
    }

    /// Input keeps its order however fast it comes, and batches while a call is in flight.
    func testInputKeepsOrder() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let conn = await online(hab)
        let pump = PaneInputPump(pane: "%1", connection: conn)
        var want = Data()
        for i in 0..<300 {
            let d = Data("\(i),".utf8)
            want.append(d)
            pump.enqueue(d)
        }
        await eventually("all input") { await hab.terminals.inputs.reduce(0) { $0 + $1.data.count } == want.count }
        let got = await hab.terminals.inputs.reduce(into: Data()) { $0.append($1.data) }
        XCTAssertEqual(got, want)
        let calls = await hab.terminals.inputs.count
        XCTAssertLessThan(calls, 300, "input typed while a call is in flight goes out in one batch")
    }

    func testReplyFilter() {
        func keep(_ s: String, owner: Bool = false) -> String {
            String(decoding: TerminalReplyFilter.keep(Data(s.utf8), owner: owner), as: UTF8.self)
        }
        XCTAssertEqual(keep("\u{1B}]52;c;aGk=\u{07}"), "")
        XCTAssertEqual(keep("\u{1B}[4;600;800t"), "")
        XCTAssertEqual(keep("\u{1B}[6;16;8t"), "")
        XCTAssertEqual(keep("\u{1B}[I"), "")
        XCTAssertEqual(keep("\u{1B}[I", owner: true), "\u{1B}[I")
        // Keys pass.
        XCTAssertEqual(keep("\u{1B}[A"), "\u{1B}[A")
        XCTAssertEqual(keep("\u{1B}"), "\u{1B}")
        XCTAssertEqual(keep("ls\r"), "ls\r")
        XCTAssertEqual(keep("\u{1B}[1;5D"), "\u{1B}[1;5D")

        // Replies to queries the hab left in the stream.
        let e = "\u{1B}"
        for reply in ["\(e)[?62;22c", "\(e)[>1;10;0c", "\(e)[?2004;1$y", "\(e)[4;2$y", "\(e)[?5u", "\(e)[0n",
                      "\(e)[8;24;80t", "\(e)[?12;40R", "\(e)]11;rgb:1e1e/1e1e/1e1e\(e)\\", "\(e)]4;1;rgb:cd/00/00\u{07}",
                      "\(e)P1$r0m\(e)\\", "\(e)P>|ghostty 1.2\(e)\\", "\(e)P1+r544e\(e)\\"] {
            XCTAssertEqual(keep(reply), "", reply.debugDescription)
            XCTAssertEqual(keep(reply, owner: true), "", reply.debugDescription)
        }
        // A cursor position report and a modified F3 look alike: only the owner's passes.
        XCTAssertEqual(keep("\(e)[12;40R"), "")
        XCTAssertEqual(keep("\(e)[1;2R", owner: true), "\(e)[1;2R")
        // Alt+Shift+P passes.
        XCTAssertEqual(keep("\(e)P"), "\(e)P")
        XCTAssertEqual(keep("\(e)Pq"), "\(e)Pq")

        // Mouse: clicks pass; motion and the wheel only for the owner.
        XCTAssertEqual(keep("\(e)[<0;10;5M"), "\(e)[<0;10;5M")
        XCTAssertEqual(keep("\(e)[<0;10;5m"), "\(e)[<0;10;5m")
        XCTAssertEqual(keep("\(e)[<35;10;5M"), "")
        XCTAssertEqual(keep("\(e)[<64;10;5M"), "")
        XCTAssertEqual(keep("\(e)[<65;10;5M", owner: true), "\(e)[<65;10;5M")
        XCTAssertEqual(keep("\(e)[M\u{43}!!"), "") // legacy motion: 32 + 35
        XCTAssertEqual(keep("\(e)[M\u{20}!!"), "\(e)[M\u{20}!!") // legacy press
    }

    /// A large paste goes out in calls of at most 64 KiB, in order.
    func testLargePasteIsSplit() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let conn = await online(hab)
        let errors = OSAllocatedUnfairLock<[String]>(initialState: [])
        let pump = PaneInputPump(pane: "%1", connection: conn, onError: { e in errors.withLock { $0.append("\(e)") } })
        var want = Data()
        for i in 0..<5 { want.append(Data(repeating: UInt8(0x41 + i), count: 500_000)) }
        pump.enqueue(want)
        await eventually("all", timeout: 30) {
            let got = await hab.terminals.inputs.reduce(0) { $0 + $1.data.count }
            return got == want.count || !errors.withLock { $0.isEmpty }
        }
        XCTAssertEqual(errors.withLock { $0 }, [], "a dropped batch")
        let inputs = await hab.terminals.inputs
        XCTAssertEqual(inputs.reduce(into: Data()) { $0.append($1.data) }, want)
        XCTAssertTrue(inputs.allSatisfy { $0.data.count <= 64 << 10 })
        XCTAssertEqual(inputs.count, (want.count + (64 << 10) - 1) / (64 << 10))
    }

    /// A 1 MiB paste over a 400 KB/s uplink with a 2 s liveness window. A batch (88 KB of
    /// base64) takes about 220 ms and its reply comes back well inside the window, so the
    /// connection stays up and the paste lands whole. One 1 MiB call (1.4 MB of base64) would
    /// take at least 3.5 s with nothing coming back, and the link would be declared dead.
    func testSlowUplinkPasteCompletes() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let uplink = SlowUplink(hab.transport(), bytesPerSecond: 400_000)
        var timing = livenessTiming()
        timing.deadAfter = .seconds(2)
        let conn = HabConnection(id: "h", transport: uplink, timing: timing, clientLabel: "test")
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        let errors = OSAllocatedUnfairLock<[String]>(initialState: [])
        let pump = PaneInputPump(pane: "%1", connection: conn, onError: { e in errors.withLock { $0.append("\(e)") } })
        let want = Data((0..<(1 << 20)).map { UInt8(0x20 + $0 % 90) })
        pump.enqueue(want)
        await eventually("all", timeout: 30) {
            let got = await hab.terminals.inputs.reduce(0) { $0 + $1.data.count }
            return got == want.count || !errors.withLock { $0.isEmpty }
        }
        XCTAssertEqual(errors.withLock { $0 }, [], "a dropped batch")
        let got = await hab.terminals.inputs.reduce(into: Data()) { $0.append($1.data) }
        XCTAssertEqual(got, want)
        let state = await conn.state
        XCTAssertEqual(state.status, .online)
        XCTAssertEqual(state.snapshots, 1, "the connection never dropped")
        await conn.stop()
    }

    /// A refused call cut through a paste's end marker: the marker is still sent next, alone,
    /// and the report does not blame the connection.
    func testDropKeepsEndMarkerCutByBatch() async {
        let sent = OSAllocatedUnfairLock<[Data]>(initialState: [])
        let drops = OSAllocatedUnfairLock<[InputDropped]>(initialState: [])
        let pump = PaneInputPump(send: { d in
            let first = sent.withLock { s -> Bool in
                defer { s.append(d) }
                return s.isEmpty
            }
            if first { throw RPCError(code: 1, message: "refused") }
        }, onError: { e in drops.withLock { $0.append(e) } })
        // The first batch ends three bytes into the end marker.
        let filler = PaneInputPump.maxBatch - PaneInputPump.pasteStart.count - 3
        pump.enqueue(PaneInputPump.pasteStart + Data(repeating: 0x41, count: filler) + PaneInputPump.pasteEnd)
        await eventually("marker sent") { sent.withLock { $0.count } == 2 }
        XCTAssertEqual(sent.withLock { $0.last }, PaneInputPump.pasteEnd)
        XCTAssertEqual(drops.withLock { $0.map(\.dropped) }, [3])
        XCTAssertEqual(drops.withLock { $0.first?.connectionLost }, false)
        XCTAssertEqual(TerminalModel.inputNoticeText(drops.withLock { $0[0] }), "Some input was not sent.")
    }

    /// The last batch of a bracketed paste fails: the earlier batches opened the paste, so
    /// the end marker goes next. A small paste that fails whole leaves nothing open, so no
    /// marker follows it.
    func testDropOwesEndMarkerOnlyForAnOpenPaste() async {
        let sent = OSAllocatedUnfairLock<[Data]>(initialState: [])
        let failOn = OSAllocatedUnfairLock<Set<Int>>(initialState: [3])
        let pump = PaneInputPump(send: { d in
            let n = sent.withLock { s -> Int in
                s.append(d)
                return s.count
            }
            if failOn.withLock({ $0.contains(n) }) { throw ConnectionClosed(detail: "gone") }
        })
        let body = Data(repeating: 0x41, count: 2 * PaneInputPump.maxBatch + 100)
        pump.enqueue(PaneInputPump.pasteStart + body + PaneInputPump.pasteEnd)
        await eventually("marker after the failed last batch") { sent.withLock { $0.count } == 4 }
        XCTAssertTrue(sent.withLock { $0[2] }.range(of: PaneInputPump.pasteEnd) != nil, "the failed batch held the end")
        XCTAssertEqual(sent.withLock { $0[3] }, PaneInputPump.pasteEnd)

        failOn.withLock { $0 = [5] }
        pump.enqueue(PaneInputPump.pasteStart + Data("hi".utf8) + PaneInputPump.pasteEnd)
        await eventually("small paste sent") { sent.withLock { $0.count } == 5 }
        pump.enqueue(Data("x".utf8))
        await eventually("next input") { sent.withLock { $0.count } == 6 }
        XCTAssertEqual(sent.withLock { $0[5] }, Data("x".utf8), "no marker after a paste that failed whole")
    }

    /// A write that makes no progress still ends the connection after the liveness window.
    /// The pump drops what waits behind the failed call and reports once. The paste was
    /// bracketed, so its end marker is kept: after the reconnect it goes out first, then the
    /// input typed since, with no other part of the dropped paste.
    func testStalledUplinkIsDead() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let uplink = SlowUplink(hab.transport())
        let conn = HabConnection(id: "h", transport: uplink, timing: livenessTiming(), clientLabel: "test")
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        let drops = OSAllocatedUnfairLock<[InputDropped]>(initialState: [])
        let pump = PaneInputPump(pane: "%1", connection: conn, onError: { e in drops.withLock { $0.append(e) } })
        uplink.setStalled(true)
        let paste = PaneInputPump.pasteStart + Data(repeating: 0x41, count: 200_000) + PaneInputPump.pasteEnd
        pump.enqueue(paste)
        await eventually("declared dead") { await conn.state.status != .online }
        await eventually("one report") { drops.withLock { $0.count } == 1 }
        XCTAssertEqual(drops.withLock { $0.first?.dropped }, paste.count - (64 << 10))
        XCTAssertEqual(drops.withLock { $0.first?.connectionLost }, true)
        uplink.setStalled(false)
        await eventually("online again") { await conn.state.status == .online }
        pump.enqueue(Data("after".utf8))
        await eventually("typed input") {
            await hab.terminals.inputs.reduce(0) { $0 + $1.data.count } == PaneInputPump.pasteEnd.count + 5
        }
        let got = await hab.terminals.inputs.reduce(into: Data()) { $0.append($1.data) }
        XCTAssertEqual(got, PaneInputPump.pasteEnd + Data("after".utf8))
        XCTAssertEqual(drops.withLock { $0.count }, 1)
        await conn.stop()
    }

    func testFrameBufferSplitsAnywhere() throws {
        let frames = [DataFrame.bytes(offset: 7, Data("xyz".utf8)), DataFrame.snapshot(offset: 9, Data("screen".utf8))]
        let wire = frames.reduce(into: Data()) { $0.append($1.encoded()) }
        for cut in 0...wire.count {
            var b = FrameBuffer()
            let got = try b.append(wire.prefix(cut)) + b.append(wire.dropFirst(cut))
            XCTAssertEqual(got, frames)
        }
        let s = try frames[1].offsetPayload(named: true)
        XCTAssertEqual(s.offset, 9)
        XCTAssertEqual(s.encoding, "snapshot-vt")
        XCTAssertEqual(s.data, Data("screen".utf8))
    }
}
