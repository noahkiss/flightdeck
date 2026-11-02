import CoreGraphics
import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

@MainActor
final class TerminalModelTests: XCTestCase {
    private func online(_ hab: FakeHab, _ label: String) async -> HabConnection {
        let conn = HabConnection(id: label, transport: hab.transport(), timing: fastTiming(), clientLabel: label)
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        return conn
    }

    private func model(_ pane: String, _ conn: HabConnection) -> TerminalModel {
        let m = TerminalModel(pane: pane, connection: conn)
        m.sizeDelay = .milliseconds(10)
        return m
    }

    /// Viewing never resizes. Typing takes the size after the debounce, and
    /// the PTY then follows this view's space. Another client typing takes it
    /// back, and this view stops sending sizes.
    func testTypingTakesTheSizeAndViewingNeverDoes() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1", cols: 80, rows: 24)
        let conn = await online(hab, "mac")
        let m = model("%1", conn)
        m.containerChanged(CGSize(width: 800, height: 600))
        m.start()
        await eventually("live") { m.status == .live }
        let want = m.sizing.desired
        XCTAssertNotEqual(want.cols, 80)

        // Viewing, a window resize, a new cell size: nothing reaches the hab.
        m.containerChanged(CGSize(width: 900, height: 640))
        try await Task.sleep(for: .milliseconds(400))
        var sizes = await hab.terminals.desiredSizes
        var resizes = await hab.terminals.resizes
        XCTAssertTrue(sizes.isEmpty)
        XCTAssertTrue(resizes.isEmpty)
        XCTAssertEqual(m.sizing.cols, 80)
        XCTAssertEqual(m.sizing.scale, 1)

        // Typing: the same path the terminal's key output takes.
        let me = await conn.state.welcome?.clientId
        await hab.terminals.setTypingDebounce(.milliseconds(300))
        let typed = Date()
        TerminalModelTests.type(m, "ls\r")
        await eventually("owner") { m.sizing.isOwner }
        XCTAssertGreaterThanOrEqual(Date().timeIntervalSince(typed), 0.25, "ownership waits for the debounce")
        let mine = m.sizing.desired
        await eventually("resized") { m.sizing.cols == mine.cols && m.sizing.rows == mine.rows }
        let owner = await hab.terminals.owner("%1")
        XCTAssertEqual(owner, me)
        resizes = await hab.terminals.resizes
        XCTAssertEqual(resizes.count, 1, "one resize per change, not per key")

        // The owner's window grows: the PTY follows.
        m.containerChanged(CGSize(width: 1000, height: 700))
        let grown = m.sizing.desired
        await eventually("grown") { m.sizing.cols == grown.cols && m.sizing.rows == grown.rows }

        // Another client types and takes the pane; this view sends nothing more.
        let phone = await online(hab, "phone")
        _ = try await phone.call(HabMethods.paneInput, InputParams(data: Data("x".utf8), pane: "%1"))
        await eventually("lost") { !m.sizing.isOwner }
        sizes = await hab.terminals.desiredSizes
        let before = sizes.count
        m.containerChanged(CGSize(width: 500, height: 400))
        try await Task.sleep(for: .milliseconds(200))
        sizes = await hab.terminals.desiredSizes
        XCTAssertEqual(sizes.count, before)
        XCTAssertLessThan(m.sizing.scale, 1, "not the owner: the pane is scaled to fit")
        m.stop()
    }

    /// Redraw is a snapshot and a nudge; it writes nothing to the program.
    func testRedrawIsANudgeAndNeverCtrlL() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let conn = await online(hab, "mac")
        let m = model("%1", conn)
        m.start()
        await eventually("live") { m.status == .live }
        m.redraw()
        await eventually("redraw") { await hab.terminals.redraws == [.nudge] }
        let inputs = await hab.terminals.inputs
        XCTAssertTrue(inputs.isEmpty)
        m.stop()
    }

    /// Output waits until libghostty's grid equals the pane's size, so rows
    /// never wrap at a wrong width. Too much held output becomes a snapshot.
    func testOutputWaitsForTheGrid() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1", cols: 100, rows: 30)
        await hab.terminals.write("%1", Data("before\r\n".utf8))
        let conn = await online(hab, "mac")
        let m = model("%1", conn)
        m.start()
        await eventually("live") { m.status == .live }
        await hab.terminals.write("%1", Data("after\r\n".utf8))
        await eventually("held") { m.heldBytes > 8 }
        m.gridForTests(cols: 80, rows: 24)
        XCTAssertGreaterThan(m.heldBytes, 0, "a grid of the wrong size gets nothing")
        m.gridForTests(cols: 100, rows: 30)
        XCTAssertEqual(m.heldBytes, 0)

        // The pane grows: output waits again for the new grid.
        m.pendingLimit = 64
        _ = try await conn.call(HabMethods.paneSetDesiredSize, DesiredSizeParams(cols: 120, pane: "%1", rows: 40))
        await eventually("new size") { m.sizing.cols == 120 }
        await hab.terminals.write("%1", Data(repeating: 0x2E, count: 200))
        try await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(m.heldBytes, 0, "past the limit the held output is dropped")
        m.gridForTests(cols: 120, rows: 40)
        await eventually("snapshot asked for") { await hab.terminals.redraws == [.snapshot] }
        m.stop()
    }

    /// A view that disappears and comes back (a push on top, a tab switch,
    /// the app in the background) opens its stream again and resumes from
    /// the offset it had applied.
    func testReappearResumesTheStream() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let conn = await online(hab, "mac")
        let m = model("%1", conn)
        m.start()
        await eventually("live") { m.status == .live }
        await hab.terminals.write("%1", Data("one ".utf8))
        let first = await hab.terminals.end("%1")
        try await Task.sleep(for: .milliseconds(100))
        m.stop()
        await eventually("closed") { await hab.terminals.dataConnectionCount == 0 }
        await hab.terminals.write("%1", Data("two".utf8))
        m.start()
        await eventually("open again") { await hab.terminals.dataConnectionCount == 1 }
        await eventually("live again") { m.status == .live }
        let hellos = await hab.terminals.hellos
        XCTAssertEqual(hellos.count, 2)
        XCTAssertEqual(hellos.last ?? nil, first, "the new stream resumes from the applied offset")
        m.stop()
    }

    /// After the control connection reconnects with a new client id, typing
    /// still takes the size: the view reads its id again.
    func testNewClientIDAfterReconnectStillTakesTheSize() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1", cols: 80, rows: 24)
        await hab.terminals.setTypingDebounce(.milliseconds(50))
        let conn = await online(hab, "mac")
        let m = model("%1", conn)
        m.containerChanged(CGSize(width: 800, height: 600))
        m.start()
        await eventually("live") { m.status == .live }
        TerminalModelTests.type(m, "a")
        await eventually("owner") { m.sizing.isOwner }
        let before = await conn.state.welcome?.clientId

        // A new gff-hab process: tokens are void, so the reconnect gets a new id.
        await hab.restart()
        await eventually("new id", timeout: 5) {
            let now = await conn.state.welcome?.clientId
            return await conn.state.status == .online && now != nil && now != before
        }
        await eventually("owner cleared") { !m.sizing.isOwner }
        m.containerChanged(CGSize(width: 640, height: 480))
        let want = m.sizing.desired
        TerminalModelTests.type(m, "b")
        await eventually("owner again") { m.sizing.isOwner }
        await eventually("resized to this view") { m.sizing.cols == want.cols && m.sizing.rows == want.rows }
        m.stop()
    }

    /// A snapshot followed at once by a resize frame was drawn at the old
    /// size; the view draws it again once the grid has the new size.
    func testSnapshotBeforeResizeIsDrawnAgain() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1", cols: 80, rows: 24)
        await hab.terminals.write("%1", Data("screen text".utf8))
        let conn = await online(hab, "mac")
        let m = model("%1", conn)
        m.start()
        await eventually("live") { m.status == .live }
        m.gridForTests(cols: 80, rows: 24)
        XCTAssertEqual(m.heldBytes, 0)
        await hab.terminals.resync("%1", cols: 100, rows: 30)
        await eventually("new size") { m.sizing.cols == 100 }
        XCTAssertGreaterThan(m.heldBytes, 11, "the snapshot waits to be drawn at 100x30")
        m.gridForTests(cols: 100, rows: 30)
        XCTAssertEqual(m.heldBytes, 0)
        m.stop()
    }

    /// In the background the fleet closes every terminal's data connection
    /// with the control connections; the foreground resumes it by offset.
    func testBackgroundClosesDataConnections() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let fleet = Fleet()
        let conn = HabConnection(id: "h", transport: hab.transport(), timing: fastTiming(), clientLabel: "phone")
        fleet.add(conn, label: nil)
        await fleet.setActive(true)
        await eventually("online") { await conn.state.status == .online }
        let m = model("%1", conn)
        fleet.terminals.add(m)
        m.start()
        await eventually("live") { m.status == .live }
        await hab.terminals.write("%1", Data("before".utf8))
        let first = await hab.terminals.end("%1")
        try await Task.sleep(for: .milliseconds(100))

        await fleet.setActive(false)
        await eventually("data closed") { await hab.terminals.dataConnectionCount == 0 }
        await eventually("nothing open") { fleet.openStreams() == 0 }
        try await Task.sleep(for: .milliseconds(300))
        let tickets = await hab.terminals.hellos.count
        XCTAssertEqual(tickets, 1, "no retries while in the background")

        await fleet.setActive(true)
        await eventually("data open", timeout: 5) { await hab.terminals.dataConnectionCount == 1 }
        await eventually("live again") { m.status == .live }
        let hellos = await hab.terminals.hellos
        XCTAssertEqual(hellos.last ?? nil, first)

        // A view that disappeared is not reopened by the foreground.
        fleet.terminals.remove(m)
        m.stop()
        await fleet.setActive(false)
        await fleet.setActive(true)
        try await Task.sleep(for: .milliseconds(200))
        let open = await hab.terminals.dataConnectionCount
        XCTAssertEqual(open, 0)
    }

    func testExitShowsStatus() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1")
        let conn = await online(hab, "mac")
        let m = model("%1", conn)
        m.start()
        await eventually("live") { m.status == .live }
        await hab.terminals.exit("%1", code: 2)
        await eventually("exited") { m.status == .exited(ExitedFrame(code: 2)) }
        XCTAssertEqual(TerminalScreen.exitText(ExitedFrame(code: 2)), "Exited with status 2")
    }

    func testSizingRule() {
        var s = PaneSizing()
        s.cell = CGSize(width: 8, height: 16)
        s.cols = 100
        s.rows = 30
        s.container = CGSize(width: 402, height: 800)
        XCTAssertEqual(s.surface.width, 804)
        XCTAssertEqual(s.scale, 0.5, accuracy: 0.001)
        // The desired grid never needs a scale: its surface fits.
        let d = s.desired
        s.cols = d.cols
        s.rows = d.rows
        XCTAssertEqual(s.scale, 1)
        XCTAssertEqual(d.cols, 49)
        XCTAssertEqual(d.rows, 49)
    }

    static func type(_ m: TerminalModel, _ s: String) {
        m.inputPumpForTests.enqueue(Data(s.utf8))
    }
}
