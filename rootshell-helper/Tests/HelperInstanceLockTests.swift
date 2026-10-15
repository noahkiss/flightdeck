import XCTest

final class HelperInstanceLockTests: XCTestCase {
    func testSecondOwnerCannotAcquireLockUntilFirstReleasesIt() throws {
        let path = FileManager.default.temporaryDirectory
            .appendingPathComponent("rootshell-helper-\(UUID().uuidString).lock").path
        defer { try? FileManager.default.removeItem(atPath: path) }

        var first: HelperInstanceLock? = try HelperInstanceLock(path: path)
        XCTAssertThrowsError(try HelperInstanceLock(path: path)) { error in
            guard case HelperInstanceLockError.alreadyRunning = error else {
                return XCTFail("Expected alreadyRunning, got \(error)")
            }
        }

        first = nil
        XCTAssertNoThrow(try HelperInstanceLock(path: path))
    }
}
