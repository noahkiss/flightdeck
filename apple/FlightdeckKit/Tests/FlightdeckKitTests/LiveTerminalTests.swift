import CoreGraphics
import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The terminal view against a real gff-hab with a holder. Skipped unless
/// `FLIGHTDECK_LIVE_SOCKET` names the hab's socket. It starts `vim` in a new
/// pane (`FLIGHTDECK_LIVE_PROGRAM` overrides, words split on spaces), views it,
/// types into it, drops the data connection and quits.
@MainActor
final class LiveTerminalTests: XCTestCase {
    private var env: [String: String] { ProcessInfo.processInfo.environment }

    func testLiveTerminal() async throws {
        guard let path = env["FLIGHTDECK_LIVE_SOCKET"], !path.isEmpty else {
            throw XCTSkip("FLIGHTDECK_LIVE_SOCKET not set")
        }
        let argv = (env["FLIGHTDECK_LIVE_PROGRAM"] ?? "vim -u NONE -N").split(separator: " ").map(String.init)
        let transport = UnixSocketTransport(path: path)

        // A creator owns the pane it made; it leaves, so the pane has no owner.
        let creator = HabConnection(id: "creator", transport: transport, timing: liveTiming(), clientLabel: "test-creator")
        await creator.start()
        await eventually("creator online", timeout: 10) { await creator.state.status == .online }
        let made = try await creator.call(HabMethods.sessionCreate, SessionCreateParams(argv: argv, cols: 80, name: "live-\(UInt32.random(in: 0...UInt32.max))", rows: 24))
        await creator.stop()
        let pane = made.pane

        let conn = HabConnection(id: "viewer", transport: transport, timing: liveTiming(), clientLabel: "test-viewer")
        await conn.start()
        await eventually("viewer online", timeout: 10) { await conn.state.status == .online }

        // A raw stream beside the model, to check the bytes.
        let raw = PaneStream(pane: pane, connection: conn)
        let got = Applied()
        await raw.start { await got.apply($0) }

        let m = TerminalModel(pane: pane, connection: conn)
        m.containerChanged(CGSize(width: 1100, height: 760))
        m.start()
        await eventually("live", timeout: 10) { m.status == .live }
        await eventually("vim drew", timeout: 10) { await got.text.contains("~") }

        // Viewing: no size change.
        try await Task.sleep(for: .seconds(1))
        var size = try await paneSize(conn, pane)
        XCTAssertEqual(size.cols, 80)
        XCTAssertEqual(size.rows, 24)
        XCTAssertFalse(m.sizing.isOwner)

        // Typing takes ownership after the debounce and sets this view's size.
        let want = m.sizing.desired
        LiveTerminalTests.type(m, "ihello from flightdeck")
        await eventually("owner", timeout: 5) { m.sizing.isOwner }
        await eventually("resized", timeout: 5) { m.sizing.cols == want.cols && m.sizing.rows == want.rows }
        size = try await paneSize(conn, pane)
        XCTAssertEqual(size.cols, want.cols)
        XCTAssertEqual(size.rows, want.rows)
        await eventually("echo", timeout: 5) { await got.text.contains("flightdeck") }

        // A dropped data connection resumes with nothing lost or repeated.
        let snapsBefore = await got.snapshots
        await raw.dropConnection()
        LiveTerminalTests.type(m, " after the drop")
        await eventually("resumed", timeout: 10) { await raw.connections == 2 }
        await eventually("after drop", timeout: 5) { await got.text.contains("drop") }
        let snapsAfter = await got.snapshots
        let trimmed = await raw.trimmed
        let overlaps = await got.overlaps
        XCTAssertEqual(snapsAfter, snapsBefore, "a resume from the held offset needs no snapshot")
        XCTAssertEqual(trimmed, 0)
        XCTAssertEqual(overlaps, 0)

        // Redraw: a snapshot on this client's streams, and nothing typed.
        m.redraw()
        await eventually("redraw snapshot", timeout: 5) { await got.snapshots == snapsAfter + 1 }

        LiveTerminalTests.type(m, "\u{1B}:q!\r")
        await eventually("exited", timeout: 10) { if case .exited = m.status { return true } else { return false } }
        let opened = await raw.connections
        let program = argv.joined(separator: " ")
        print("live terminal: pane \(pane) \(program) owner size \(want.cols)x\(want.rows), \(opened) data connections, \(snapsAfter + 1) snapshots")
        await raw.close()
        m.stop()
        await conn.stop()
    }

    private func paneSize(_ conn: HabConnection, _ pane: String) async throws -> (cols: Int, rows: Int) {
        let s = try await conn.call(HabMethods.structureGet, StructureGetParams())
        let p = try XCTUnwrap(s.panes.first { $0.id == pane })
        return (p.cols, p.rows)
    }

    private func liveTiming() -> HabTiming {
        var t = HabTiming()
        t.callTimeout = .seconds(10)
        return t
    }

    static func type(_ m: TerminalModel, _ s: String) {
        m.inputPumpForTests.enqueue(Data(s.utf8))
    }
}
