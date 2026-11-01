import XCTest
@testable import FlightdeckKit

final class AppInfoTests: XCTestCase {
    func testDisplayNameFallsBack() {
        XCTAssertFalse(AppInfo.displayName(Bundle(for: Self.self)).isEmpty)
    }

    func testAPIRevisionComesFromHabAPI() {
        XCTAssertGreaterThanOrEqual(AppInfo.apiRevision, 1)
    }
}
