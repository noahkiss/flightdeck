import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Reopen where the owner left it (app.md 3.12).
final class SessionRestoreTests: XCTestCase {
    func testRestoreWaitsForTheRowWithinTheLimit() throws {
        XCTAssertNil(SessionRestore(saved: "", now: Date()), "nothing was open")
        let start = Date()
        let r = try XCTUnwrap(SessionRestore(saved: "bench/s/s1", now: start))
        XCTAssertNil(r.row(in: [], now: start), "the board has not listed it yet")
        let rows = [boardRow("bench/s/s2", session: "s2"), boardRow("bench/s/s1")]
        XCTAssertEqual(r.row(in: rows, now: start.addingTimeInterval(5))?.id, "bench/s/s1")
        XCTAssertFalse(r.expired(now: start.addingTimeInterval(SessionRestore.limit)))
        let late = start.addingTimeInterval(SessionRestore.limit + 1)
        XCTAssertTrue(r.expired(now: late))
        XCTAssertNil(r.row(in: rows, now: late), "a row that comes later opens nothing")
    }

    func testTheCapcomMarkMatchesNoRow() throws {
        let r = try XCTUnwrap(SessionRestore(saved: SessionRestore.capcom, now: Date()))
        XCTAssertNil(r.row(in: [boardRow("bench/s/s1")], now: Date()))
    }
}
