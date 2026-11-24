import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Menus and shortcuts (app.md 3.13) and the per-window restore (app.md 3.12).
@MainActor
final class WindowCommandTests: XCTestCase {
    private func board(online: Bool = true) -> Board {
        let b = Board()
        b.register(id: "h", label: nil, endpoint: "fake")
        b.update("h", linkHabState(host: "h", online: online, agents: [
            ("%1", "s1", .working, nil), ("%2", "s2", .blocked, .permission), ("%3", "s3", .done, nil),
        ]))
        return b
    }

    func testNothingOpenOffersTheBoardOnly() {
        let b = board()
        let w = WindowCommands(kind: .board)
        w.goTargets = [.sessions, .capcom]
        let s = w.state(board: b, actions: SessionActionCenter(board: b, connection: { _ in nil }), multipleWindows: true)
        XCTAssertTrue(s.enabled(.newSession))
        XCTAssertTrue(s.enabled(.find))
        XCTAssertTrue(s.enabled(.goCapcom))
        XCTAssertFalse(s.enabled(.goRounds), "the Mac has no Rounds tab")
        XCTAssertTrue(s.enabled(.nextNeedingYou))
        XCTAssertTrue(s.enabled(.markAllRead))
        for c: MenuCommand in [.openInNewWindow, .showChat, .showTerminal, .voice, .stop, .console, .rename, .restart,
                               .copyName, .copyLink, .close, .markRead, .markUnread] {
            XCTAssertFalse(s.enabled(c), "\(c) needs an open session")
        }
    }

    func testOpenSessionAndItsControls() {
        let b = board()
        let w = WindowCommands(kind: .board)
        w.openRowID = "h/s/s1"
        var c = SessionControls(target: SessionTarget(habID: "h", session: "s1", pane: "%1"))
        c.mode = .chat
        c.working = true
        c.voiceAllowed = true
        c.hasConsole = true
        w.session = c
        let s = w.state(board: b, actions: SessionActionCenter(board: b, connection: { _ in nil }), multipleWindows: true)
        XCTAssertTrue(s.enabled(.openInNewWindow))
        XCTAssertFalse(s.enabled(.showChat), "the chat shows already")
        XCTAssertTrue(s.enabled(.showTerminal))
        XCTAssertTrue(s.enabled(.stop))
        XCTAssertTrue(s.enabled(.voice))
        XCTAssertTrue(s.enabled(.console))
        XCTAssertTrue(s.enabled(.rename))
        XCTAssertTrue(s.enabled(.close))
        XCTAssertTrue(s.enabled(.copyLink))
        XCTAssertFalse(s.enabled(.restart), "no restart capability, and the agent works")
        XCTAssertFalse(s.enabled(.markRead), "not done")

        // Side by side has no switch; one window only has no new window.
        w.session?.mode = nil
        let one = w.state(board: b, actions: SessionActionCenter(board: b, connection: { _ in nil }), multipleWindows: false)
        XCTAssertFalse(one.enabled(.showTerminal))
        XCTAssertFalse(one.enabled(.openInNewWindow))
        XCTAssertFalse(one.enabled(.newWindow))
    }

    func testOfflineSessionOffersCopiesOnly() {
        let b = board(online: false)
        let w = WindowCommands(kind: .board)
        w.openRowID = "h/s/s3"
        let s = w.state(board: b, actions: SessionActionCenter(board: b, connection: { _ in nil }), multipleWindows: true)
        XCTAssertFalse(s.enabled(.newSession), "no hab online")
        XCTAssertFalse(s.enabled(.rename))
        XCTAssertFalse(s.enabled(.close))
        XCTAssertTrue(s.enabled(.copyName))
        XCTAssertTrue(s.enabled(.copyLink))
        XCTAssertFalse(s.enabled(.nextNeedingYou), "offline rows are skipped")
    }

    func testReadMarksFollowTheRow() {
        let b = board()
        let w = WindowCommands(kind: .board)
        w.openRowID = "h/s/s3"
        let actions = SessionActionCenter(board: b, connection: { _ in nil })
        XCTAssertTrue(w.state(board: b, actions: actions, multipleWindows: true).enabled(.markRead))
        b.markSeen(b.rows.first { $0.id == "h/s/s3" }!)
        let s = w.state(board: b, actions: actions, multipleWindows: true)
        XCTAssertFalse(s.enabled(.markRead))
        XCTAssertTrue(s.enabled(.markUnread))
    }

    func testSessionWindowLeavesTheBoardCommandsOff() {
        let b = board()
        let w = WindowCommands(kind: .session)
        w.openRowID = "h/s/s1"
        let s = w.state(board: b, actions: SessionActionCenter(board: b, connection: { _ in nil }), multipleWindows: true)
        XCTAssertFalse(s.enabled(.newSession))
        XCTAssertFalse(s.enabled(.find))
        XCTAssertFalse(s.enabled(.nextNeedingYou))
        XCTAssertFalse(s.enabled(.openInNewWindow), "it is its own window already")
        XCTAssertTrue(s.enabled(.rename))
    }

    /// Two windows: one window's opens and approval sheet leave the other's restore alone.
    func testOpensCountPerWindow() {
        let b = board()
        let a = UUID(), c = UUID()
        b.open(b.rows[0], window: a)
        b.open(b.rows[0], window: a)
        XCTAssertEqual(b.opens(in: a), 2)
        XCTAssertEqual(b.opens(in: c), 0)
        b.noteOpen(window: c)
        XCTAssertEqual(b.opens(in: c), 1)

        let approvals = ApprovalCenter(board: b, answerer: ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in throw TransportError("x") }, clientLabel: "phone"),
                                       defaults: emptySuite("window-command-tests"))
        approvals.boardChanged()
        let blocked = b.rows.first { $0.id == "h/s/s2" }!
        approvals.open(blocked, window: a)
        XCTAssertTrue(approvals.presenting)
        XCTAssertTrue(approvals.interrupts(window: a))
        XCTAssertFalse(approvals.interrupts(window: c), "a sheet raised in another window")
        approvals.presenting = false
        XCTAssertFalse(approvals.interrupts(window: a))
        approvals.open(SessionTarget(habID: "h", session: "s2", pane: "%2"))
        XCTAssertTrue(approvals.presenting)
        XCTAssertTrue(approvals.interrupts(window: c), "a sheet from outside cancels every window's restore")
    }
}
