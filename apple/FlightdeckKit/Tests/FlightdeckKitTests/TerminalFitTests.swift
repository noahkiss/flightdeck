import CoreGraphics
import Foundation
import HabAPI
import SwiftUI
import XCTest
@testable import FlightdeckKit

/// The terminal view's fit rule, its key bar's modifiers and its size modes
/// (app.md 5.4, 5.5).
@MainActor
final class TerminalFitTests: XCTestCase {
    // MARK: - The font

    func testTheFontFollowsDynamicTypeWithinBounds() {
        let phone: CGFloat = 402 // iPhone 17 Pro, portrait
        XCTAssertEqual(TerminalFit.textScale(.large), 1)
        XCTAssertEqual(TerminalFit.fontSize(width: phone, textScale: TerminalFit.textScale(.large)), 13)
        // Larger text grows the font by the body style's factor (13 x 19/17 = 14.5).
        XCTAssertEqual(TerminalFit.fontSize(width: phone, textScale: TerminalFit.textScale(.xLarge)), 14.5)
        // Smaller text never goes under the readable minimum (13 x 14/17 = 10.7).
        XCTAssertEqual(TerminalFit.fontSize(width: phone, textScale: TerminalFit.textScale(.xSmall)), TerminalFit.minPoints)
        // AX5 would be 40 pt: the 40-column cap holds it at 402 / (40 x 0.6) = 16.75, in half points.
        XCTAssertEqual(TerminalFit.fontSize(width: phone, textScale: TerminalFit.textScale(.accessibility5)), 16.5)
        // A wide iPad column reaches the 24 pt ceiling first.
        XCTAssertEqual(TerminalFit.fontSize(width: 900, textScale: TerminalFit.textScale(.accessibility5)), TerminalFit.maxPoints)
        // Too narrow for 40 columns even at the minimum: the minimum wins.
        XCTAssertEqual(TerminalFit.fontSize(width: 200, textScale: 1), TerminalFit.minPoints)
        // A measured cell aspect replaces the estimate.
        XCTAssertEqual(TerminalFit.fontSize(width: phone, textScale: 3, cellAspect: 0.5), 20)
    }

    func testTheGridFillsTheViewLessHalfACell() {
        let g = TerminalFit.grid(container: CGSize(width: 402, height: 600), cell: CGSize(width: 7.8, height: 16))
        XCTAssertEqual(g?.cols, 51)
        XCTAssertEqual(g?.rows, 37)
        // The columns for each font on a phone: at the default about 50, at AX5 at least 40.
        for scale in [1, TerminalFit.textScale(.accessibility5)] {
            let f = TerminalFit.fontSize(width: 402, textScale: scale)
            let cols = TerminalFit.grid(container: CGSize(width: 402, height: 600), cell: CGSize(width: f * 0.6, height: f * 1.2))?.cols ?? 0
            XCTAssertGreaterThanOrEqual(cols, TerminalFit.minColumns)
        }
        let tiny = TerminalFit.grid(container: CGSize(width: 5, height: 5), cell: CGSize(width: 7.8, height: 16))
        XCTAssertEqual(tiny?.cols, 2)
        XCTAssertEqual(tiny?.rows, 1)
        XCTAssertNil(TerminalFit.grid(container: .zero, cell: CGSize(width: 7.8, height: 16)))
    }

    // MARK: - Modifiers

    func testModifiersArmLockAndRelease() {
        var k = StickyModifiers()
        let t0 = Date(timeIntervalSince1970: 1000)
        k.tap(.ctrl, at: t0)
        XCTAssertEqual(k.ctrl, .armed)
        XCTAssertEqual(k.opt, .off)
        // The next key takes Ctrl and an armed key turns off.
        XCTAssertEqual(k.consume(), [.ctrl])
        XCTAssertEqual(k.ctrl, .off)

        // A double tap within 400 ms locks; a locked key survives keys.
        k.tap(.ctrl, at: t0.addingTimeInterval(1))
        k.tap(.ctrl, at: t0.addingTimeInterval(1.3))
        XCTAssertEqual(k.ctrl, .locked)
        XCTAssertEqual(k.consume(), [.ctrl])
        XCTAssertEqual(k.consume(), [.ctrl])
        XCTAssertEqual(k.ctrl, .locked)
        // A tap on a locked key turns it off.
        k.tap(.ctrl, at: t0.addingTimeInterval(5))
        XCTAssertEqual(k.ctrl, .off)
        XCTAssertEqual(k.consume(), [])

        // A late second tap turns an armed key off instead of locking it.
        k.tap(.opt, at: t0.addingTimeInterval(10))
        k.tap(.opt, at: t0.addingTimeInterval(10.5))
        XCTAssertEqual(k.opt, .off)

        // Both at once: a key takes both; only the armed one turns off.
        k.tap(.opt, at: t0.addingTimeInterval(20))
        k.tap(.ctrl, at: t0.addingTimeInterval(21))
        k.tap(.ctrl, at: t0.addingTimeInterval(21.1))
        XCTAssertEqual(k.consume(), [.ctrl, .opt])
        XCTAssertEqual(k.ctrl, .locked)
        XCTAssertEqual(k.opt, .off)

        // VoiceOver reads each state.
        XCTAssertEqual(StickyModifiers.State.off.spoken, "Off")
        XCTAssertEqual(StickyModifiers.State.armed.spoken, "On for the next key")
        XCTAssertEqual(StickyModifiers.State.locked.spoken, "Locked")
        XCTAssertEqual(StickyHint.text(.ctrl), "Ctrl is on for the next key. Tap twice to lock it.")
    }

    func testAHardwareKeyboardReleasesEveryModifier() {
        var k = StickyModifiers()
        let t0 = Date(timeIntervalSince1970: 1000)
        k.tap(.ctrl, at: t0)
        k.tap(.ctrl, at: t0.addingTimeInterval(0.1))
        k.tap(.opt, at: t0.addingTimeInterval(0.2))
        XCTAssertTrue(k.anyActive)
        k.hardwareKeyboardAttached()
        XCTAssertEqual(k.ctrl, .off)
        XCTAssertEqual(k.opt, .off)
        XCTAssertEqual(k.consume(), [])
        // After it the first tap arms again; it is not a second tap.
        k.tap(.ctrl, at: t0.addingTimeInterval(0.3))
        XCTAssertEqual(k.ctrl, .armed)
    }

    func testSyncTakesWhatLibghosttySpent() {
        var k = StickyModifiers()
        k.tap(.ctrl)
        k.tap(.opt)
        k.tap(.opt)
        // A key from the system keyboard: libghostty spent the armed Ctrl.
        k.sync(ctrl: .off, opt: .locked)
        XCTAssertEqual(k.ctrl, .off)
        XCTAssertEqual(k.opt, .locked)
    }

    // MARK: - Size modes

    private func online(_ hab: FakeHab, _ label: String) async -> HabConnection {
        let conn = HabConnection(id: label, transport: hab.transport(), timing: fastTiming(), clientLabel: label)
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        return conn
    }

    private func phoneModel(_ pane: String, _ conn: HabConnection) -> TerminalModel {
        let m = TerminalModel(pane: pane, connection: conn)
        m.sizeDelay = .milliseconds(10)
        m.requestOnOpen = true
        m.noticeDuration = .seconds(30)
        m.containerChanged(CGSize(width: 402, height: 600))
        return m
    }

    /// Opening an unowned pane on the phone takes it with `pane.request` and
    /// fits it; Host size gives it back at the host's size, and typing then
    /// keeps that size.
    func testFittedTakesAnUnownedPaneAndHostSizeGivesItBack() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1", cols: 100, rows: 30)
        await hab.terminals.setTypingDebounce(.milliseconds(50))
        let conn = await online(hab, "phone")
        let me = await conn.state.welcome?.clientId
        let m = phoneModel("%1", conn)
        m.start()
        let want = m.sizing.desired
        await eventually("fitted") { m.sizing.cols == want.cols && m.sizing.rows == want.rows }
        XCTAssertTrue(m.sizing.isOwner)
        let owner = await hab.terminals.owner("%1")
        XCTAssertEqual(owner, me)
        XCTAssertEqual(m.sizing.label, .fitted(GridSize(cols: want.cols, rows: want.rows)))
        XCTAssertEqual(m.sizing.label.text, "Fitted · \(want.cols) × \(want.rows)")
        XCTAssertEqual(m.sizing.hostSize, GridSize(cols: 100, rows: 30), "the host's size is kept for Host size")

        m.useHostSize()
        await eventually("host size") { m.sizing.cols == 100 && m.sizing.rows == 30 }
        await eventually("given back") { await hab.terminals.owner("%1") == nil }
        await eventually("notice") { m.notice?.text == "Host size · 100 × 30" }
        XCTAssertEqual(m.sizing.label, .host(GridSize(cols: 100, rows: 30)))

        // Typing takes ownership again, but Host size keeps the pane's size.
        let resizes = await hab.terminals.resizes.count
        TerminalModelTests.type(m, "x")
        await eventually("owner") { m.sizing.isOwner }
        try await Task.sleep(for: .milliseconds(200))
        XCTAssertEqual(m.sizing.cols, 100)
        let after = await hab.terminals.resizes.count
        XCTAssertEqual(after, resizes, "typing in Host size never resizes")

        // A new rotation in Host size sends nothing that changes the pane.
        m.containerChanged(CGSize(width: 874, height: 300))
        try await Task.sleep(for: .milliseconds(100))
        XCTAssertEqual(m.sizing.cols, 100)
        m.stop()
    }

    /// A pane another client owns is never resized by opening. The control
    /// says another device sets the size; Fit takes it and says so.
    func testAnotherClientsPaneWaitsForFit() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1", cols: 120, rows: 40)
        let computer = await online(hab, "computer")
        _ = try await computer.call(HabMethods.paneTake, PaneRef(pane: "%1"))
        let conn = await online(hab, "phone")
        let m = phoneModel("%1", conn)
        m.start()
        await eventually("live") { m.status == .live }
        await eventually("owner known") { m.sizing.owner != nil }
        try await Task.sleep(for: .milliseconds(150))
        var resizes = await hab.terminals.resizes
        XCTAssertTrue(resizes.isEmpty, "opening never resizes another client's pane")
        XCTAssertEqual(m.sizing.label, .otherDevice(GridSize(cols: 120, rows: 40)))
        XCTAssertEqual(m.sizing.label.text, "Host size · 120 × 40")
        XCTAssertEqual(m.sizing.label.note, "Another device sets this size.")

        m.fitToScreen()
        let want = m.sizing.desired
        await eventually("fitted") { m.sizing.isOwner && m.sizing.cols == want.cols && m.sizing.rows == want.rows }
        await eventually("notice") { m.notice?.text == "Fitted · \(want.cols) × \(want.rows)" }
        resizes = await hab.terminals.resizes
        XCTAssertEqual(resizes.count, 1)

        // Redraw says so when the hab answers.
        m.redraw()
        await eventually("redrawn") { m.notice?.text == "Redrawn" }
        m.stop()
    }

    /// The Mac never takes a pane on open (gff-client.md and app.md 5.4).
    func testNoRequestWhenOpeningOnTheMac() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1", cols: 100, rows: 30)
        let conn = await online(hab, "mac")
        let m = phoneModel("%1", conn)
        m.requestOnOpen = false
        m.start()
        await eventually("live") { m.status == .live }
        try await Task.sleep(for: .milliseconds(150))
        let owner = await hab.terminals.owner("%1")
        XCTAssertNil(owner)
        XCTAssertEqual(m.sizing.label, .host(GridSize(cols: 100, rows: 30)))
        m.stop()
    }

    /// A refused size says why in plain words.
    func testRefusalsInPlainWords() {
        func rpc(_ kind: ErrorKind) -> RPCError {
            RPCError(code: 1, message: "", data: ErrorData(hint: "x", kind: kind, seq: 0))
        }
        XCTAssertEqual(TerminalModel.refusalText(rpc(.notFound)), "This terminal is gone.")
        XCTAssertEqual(TerminalModel.refusalText(rpc(.invalid)), "This view only watches the terminal, so it can't change its size.")
        XCTAssertEqual(TerminalModel.refusalText(ConnectionClosed()), "Not connected. Try again when the hab is back.")
        XCTAssertEqual(TerminalModel.refusalText(CallTimedOut(method: "pane.take")), "Not connected. Try again when the hab is back.")
        XCTAssertEqual(TerminalModel.refusalText(rpc(.unavailable)), "The hab refused the size.")
        XCTAssertEqual(TerminalModel.refusalText(rpc(.unavailable), redraw: true), "The hab refused the redraw.")
        XCTAssertEqual(TerminalModel.keptByOther, "Another device has this terminal and kept its size.")
    }

    /// A pane gone between opening and Fit: the notice names it.
    func testFitOnAGonePaneSaysSo() async throws {
        let hab = FakeHab(hostID: "box")
        await hab.terminals.add("%1", cols: 100, rows: 30)
        let conn = await online(hab, "phone")
        let m = phoneModel("%9", conn)
        m.fitToScreen()
        await eventually("notice") { m.notice?.text == "This terminal is gone." }
        XCTAssertEqual(m.notice?.isError, true)
    }

    /// The demo's agent screen fits any width: no line is wider than the pane.
    func testDemoAgentScreenFitsItsWidth() {
        for cols in [40, 50, 85, 120] {
            let screen = DemoTerminal.agentScreen(cols: cols, rows: 40)
            let plain = screen.replacingOccurrences(of: "\u{1B}\\[[0-9;]*m", with: "", options: .regularExpression)
            for line in plain.components(separatedBy: "\r\n") {
                XCTAssertLessThanOrEqual(line.count, cols, "\(cols): \(line)")
            }
            XCTAssertTrue(plain.contains("npm run migrate"))
        }
    }
}
