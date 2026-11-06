import Foundation
import XCTest

/// A value set from another task.
final class Box<T: Sendable>: @unchecked Sendable {
    private let lock = NSLock()
    private var value: T?
    func set(_ v: T) { lock.withLock { value = v } }
    func get() -> T? { lock.withLock { value } }
}

func XCTAssertThrowsAsync<T>(_ body: @autoclosure () async throws -> T, file: StaticString = #filePath, line: UInt = #line, _ check: (Error) -> Void = { _ in }) async {
    do {
        _ = try await body()
        XCTFail("no error", file: file, line: line)
    } catch {
        check(error)
    }
}
