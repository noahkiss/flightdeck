import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Next needing you and unread (app.md 3.16).
@MainActor
final class NextNeedingYouTests: XCTestCase {
    func testNextWrapsInBoardOrderAndSkipsOffline() {
        let needs = [boardRow("h/s/a", rank: .blocked), boardRow("h/s/b", rank: .error), boardRow("h/s/c", rank: .doneUnseen)]
        var off = boardRow("h/s/d", rank: .blocked)
        off.offline = true
        let sections = [BoardSection(kind: .needsYou, rows: needs + [off]),
                        BoardSection(kind: .working, rows: [boardRow("h/s/w", rank: .working)])]
        XCTAssertEqual(NextNeedingYou.candidates(sections).map(\.id), ["h/s/a", "h/s/b", "h/s/c"])
        XCTAssertEqual(NextNeedingYou.next(in: sections, after: nil)?.id, "h/s/a", "none open: the first")
        XCTAssertEqual(NextNeedingYou.next(in: sections, after: "h/s/a")?.id, "h/s/b")
        XCTAssertEqual(NextNeedingYou.next(in: sections, after: "h/s/c")?.id, "h/s/a", "wraps round")
        XCTAssertEqual(NextNeedingYou.next(in: sections, after: "h/s/w")?.id, "h/s/a", "an open row that needs nothing")
        XCTAssertEqual(NextNeedingYou.count(in: sections, excluding: "h/s/b"), 2)

        let one = [BoardSection(kind: .needsYou, rows: [boardRow("h/s/a", rank: .blocked)])]
        XCTAssertNil(NextNeedingYou.next(in: one, after: "h/s/a"), "only the open one needs you")
        XCTAssertEqual(NextNeedingYou.count(in: one, excluding: "h/s/a"), 0)
        XCTAssertNil(NextNeedingYou.next(in: [], after: nil))
    }

    func testMarkReadAndUnread() {
        let board = Board()
        board.register(id: "h", label: nil, endpoint: "fake")
        board.update("h", linkHabState(host: "h", agents: [("%1", "s1", .done, nil), ("%2", "s2", .done, nil), ("%3", "s3", .idle, nil)]))
        func rank(_ s: String) -> BoardRank? { board.rows.first { $0.target.session == s }?.rank }
        func row(_ s: String) -> BoardRow { board.rows.first { $0.target.session == s }! }
        XCTAssertEqual(board.unreadCount, 2)
        board.markSeen(row("s1"))
        XCTAssertEqual(rank("s1"), .atRest)
        board.markUnseen(row("s1"))
        XCTAssertEqual(rank("s1"), .doneUnseen, "back in Needs you")
        XCTAssertFalse(board.canMark(row("s3")), "an idle session has nothing to mark")
        board.markUnseen(row("s3"))
        XCTAssertEqual(rank("s3"), .atRest)
        board.markAllSeen()
        XCTAssertEqual(board.unreadCount, 0)
        XCTAssertEqual(board.needsYouCount, 0)
    }
}
