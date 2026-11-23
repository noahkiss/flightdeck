import Foundation
import Synchronization
import XCTest

/// A value set from another task.
final class Box<T: Sendable>: Sendable {
    private let value = Mutex<T?>(nil)
    func set(_ v: T) { value.withLock { $0 = v } }
    func get() -> T? { value.withLock { $0 } }
}

func XCTAssertThrowsAsync<T>(_ body: @autoclosure () async throws -> T, file: StaticString = #filePath, line: UInt = #line, _ check: (Error) -> Void = { _ in }) async {
    do {
        _ = try await body()
        XCTFail("no error", file: file, line: line)
    } catch {
        check(error)
    }
}
