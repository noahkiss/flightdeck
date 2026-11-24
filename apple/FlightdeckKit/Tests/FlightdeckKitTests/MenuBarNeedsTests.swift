import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The needs-you menu in the Mac menu bar (app.md 3.17).
@MainActor
final class MenuBarNeedsTests: XCTestCase {
    private func board(_ agents: [(pane: String, session: String, state: AgentStatus, kind: BlockedKind?)], online: Bool = true) -> Board {
        let b = Board()
        b.register(id: "h", label: "bench", endpoint: "fake")
        b.update("h", linkHabState(host: "h", online: online, agents: agents))
        return b
    }

    func testListsNeedsYouInBoardOrderWithTheirReasons() throws {
        let b = board([("%1", "s1", .blocked, .permission), ("%2", "s2", .working, nil), ("%3", "s3", .done, nil),
                       ("%4", "s4", .blocked, .question), ("%5", "s5", .error, nil)])
        guard case .needs(let items, let confirmations) = MenuBarNeeds.content(b) else { return XCTFail("needs") }
        XCTAssertEqual(items.map(\.row.id), NextNeedingYou.candidates(b.sections).map(\.id), "the rows ⌥⌘A moves through")
        XCTAssertEqual(items.map(\.row.rank), [.blocked, .blocked, .error, .doneUnseen], "blocked, error, then done unseen")
        let by = Dictionary(uniqueKeysWithValues: items.map { ($0.row.target.session ?? "", $0) })
        XCTAssertEqual(by["s1"]?.detail, "Needs permission · bench")
        XCTAssertEqual(by["s4"]?.detail, "Asks a question · bench")
        XCTAssertEqual(by["s5"]?.detail, "Stopped with an error · bench")
        XCTAssertEqual(by["s3"]?.detail, "Done · bench")
        XCTAssertEqual(by["s1"]?.symbol, "hand.raised")
        XCTAssertEqual(by["s4"]?.symbol, "questionmark.bubble")
        XCTAssertEqual(by["s5"]?.symbol, "exclamationmark.triangle")
        XCTAssertEqual(by["s3"]?.symbol, "checkmark.circle")
        XCTAssertEqual(confirmations, 0)
        XCTAssertEqual(MenuBarNeeds.count(MenuBarNeeds.content(b)), 4)
        XCTAssertEqual(MenuBarNeeds.count(MenuBarNeeds.content(b)), b.needsYouCount, "the Dock badge's count")
        XCTAssertEqual(MenuBarNeeds.symbol(MenuBarNeeds.content(b)), "bell.badge.fill")
    }

    func testReadSessionsLeaveTheMenu() {
        let b = board([("%1", "s1", .done, nil)])
        XCTAssertEqual(MenuBarNeeds.count(MenuBarNeeds.content(b)), 1)
        b.markAllSeen()
        XCTAssertEqual(MenuBarNeeds.content(b), .needs([], confirmations: 0))
        XCTAssertNil(MenuBarNeeds.count(MenuBarNeeds.content(b)), "no count at 0")
        XCTAssertEqual(MenuBarNeeds.symbol(MenuBarNeeds.content(b)), "bell")
    }

    /// With every window closed the Mac holds no connection (12.1): the menu says so, never 0.
    func testNotConnectedAndNoHabs() {
        let off = board([("%1", "s1", .blocked, .permission)], online: false)
        XCTAssertEqual(MenuBarNeeds.content(off), .notConnected)
        XCTAssertNil(MenuBarNeeds.count(.notConnected))
        XCTAssertEqual(MenuBarNeeds.content(Board()), .noHabs)
    }

    func testConfirmationsLine() {
        XCTAssertEqual(MenuBarNeeds.confirmationsLine(1), "1 confirmation waits")
        XCTAssertEqual(MenuBarNeeds.confirmationsLine(3), "3 confirmations wait")
        XCTAssertEqual(MenuBarNeeds.count(.needs([], confirmations: 2)), 2)
    }
}
