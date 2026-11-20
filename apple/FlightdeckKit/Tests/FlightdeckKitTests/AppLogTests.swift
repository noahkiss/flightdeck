import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The app's logging wrapper (diagnostics.md 4.10, test 14): a value is private unless its
/// kind is safe, and app.log keeps notice and above, redacted, within its cap.
final class AppLogTests: XCTestCase {
    private enum Choice: String { case allow }
    private enum Failure: Error { case refused(String), gone }

    func testStringDefaultsToPrivate() {
        let host = "build-box.example"
        let m: AppLogMessage = "connect to \(host) failed"
        XCTAssertEqual(m.redacted, "connect to <private> failed")
        XCTAssertEqual(m.full, "connect to build-box.example failed")
        XCTAssertTrue(m.hasPrivate)
    }

    func testAnyValueDefaultsToPrivate() {
        let url = URL(fileURLWithPath: "/tmp/a")
        let err = Failure.refused("a user's words")
        let m: AppLogMessage = "\(url) \(err) \(Optional("x") as Any)"
        XCTAssertEqual(m.redacted, "<private> <private> <private>")
    }

    func testSafeKindsArePublic() {
        let n = 3
        let ms: Int64 = 120
        let secs = 1.5
        let m: AppLogMessage = "\(n) habs, \(ms) ms, \(secs) s, \(true), \(word: Choice.allow), \(word: "answered"), pane \(id: "%3")"
        XCTAssertEqual(m.redacted, "3 habs, 120 ms, 1.5 s, true, allow, answered, pane %3")
        XCTAssertFalse(m.hasPrivate)
    }

    func testErrorKindHoldsNoDescription() {
        let m: AppLogMessage = "failed: \(kind: Failure.refused("secret words"))"
        XCTAssertEqual(m.redacted, "failed: Failure.refused")
        XCTAssertEqual(AppLogMessage.kind(of: Failure.gone), "Failure.gone")
        let ns = NSError(domain: NSURLErrorDomain, code: -1001, userInfo: [NSLocalizedDescriptionKey: "host x timed out"])
        XCTAssertEqual(AppLogMessage.kind(of: ns), "NSURLErrorDomain -1001")
        struct Plain: Error { let detail: String }
        XCTAssertEqual(AppLogMessage.kind(of: Plain(detail: "y")), "Plain")
    }

    /// The app's struct errors name a useful kind: the API error kind, the timed-out method, or
    /// Foundation's domain and code; never the hint, the message or the URL.
    func testStructErrorKinds() {
        let rpc = RPCError(code: 1, message: "on build-box", data: ErrorData(hint: "/home/x: gone", kind: .unavailable, seq: 0))
        XCTAssertEqual(AppLogMessage.kind(of: rpc), "RPCError unavailable")
        XCTAssertEqual(AppLogMessage.kind(of: RPCError(code: -32601, message: "no method", data: nil)), "RPCError code -32601")
        XCTAssertEqual(AppLogMessage.kind(of: CallTimedOut(method: "push.register")), "timeout push.register")
        let url = URLError(.timedOut, userInfo: [NSURLErrorFailingURLStringErrorKey: "https://build-box.example/x"])
        XCTAssertEqual(AppLogMessage.kind(of: url), "NSURLErrorDomain -1001")
        XCTAssertEqual(AppLogMessage.kind(of: POSIXError(.ECONNREFUSED)), "NSPOSIXErrorDomain 61")
        let m: AppLogMessage = "push.register failed: \(kind: rpc) \(rpc)"
        XCTAssertEqual(m.redacted, "push.register failed: RPCError unavailable <private>")
    }

    private func tempFile() throws -> URL {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("applog-\(UUID().uuidString)")
        addTeardownBlock { try? FileManager.default.removeItem(at: dir) }
        return dir.appendingPathComponent("diagnostics/app.log")
    }

    private func lines(_ url: URL) throws -> [AppLogLine] {
        let text = try String(contentsOf: url, encoding: .utf8)
        return try text.split(separator: "\n").map { try JSONDecoder().decode(AppLogLine.self, from: Data($0.utf8)) }
    }

    func testFileHoldsRedactedLines() throws {
        let url = try tempFile()
        let f = AppLogFile(url: url)
        let host = "build-box.example"
        let m: AppLogMessage = "hab \(host) skipped: \(kind: Failure.gone)"
        f.write(.notice, category: "config", message: m.redacted)
        f.flush()
        let got = try lines(url)
        XCTAssertEqual(got.count, 1)
        XCTAssertEqual(got[0].lvl, "notice")
        XCTAssertEqual(got[0].cat, "config")
        XCTAssertEqual(got[0].msg, "hab <private> skipped: Failure.gone")
        XCTAssertFalse(try String(contentsOf: url, encoding: .utf8).contains(host))
        let attrs = try FileManager.default.attributesOfItem(atPath: url.path)
        XCTAssertEqual((attrs[.posixPermissions] as? NSNumber)?.intValue, 0o600)
    }

    func testFileRotatesAtItsCap() throws {
        let url = try tempFile()
        let f = AppLogFile(url: url, max: 400)
        for i in 0 ..< 20 {
            f.write(.error, category: "rpc", message: "line \(i)")
        }
        f.flush()
        let cur = try lines(url), old = try lines(f.oldURL)
        XCTAssertFalse(cur.isEmpty)
        XCTAssertFalse(old.isEmpty)
        XCTAssertEqual(cur.last?.msg, "line 19")
        for u in [url, f.oldURL] {
            let size = try FileManager.default.attributesOfItem(atPath: u.path)[.size] as? Int ?? 0
            XCTAssertLessThanOrEqual(size, 400)
        }
        XCTAssertFalse(FileManager.default.fileExists(atPath: url.appendingPathExtension("2").path), "one old file only")
    }

    /// Lines below notice never reach the file; notice and above do, redacted.
    func testOnlyNoticeAndAboveReachTheFile() throws {
        XCTAssertNil(AppLogFile.shared, "tests must not open the app's own file")
        let url = try tempFile()
        let f = AppLogFile(url: url)
        let log = AppLog(category: "test", file: f)
        let label = "my laptop"
        log.debug("debug \(label)")
        log.info("info \(label)")
        log.notice("notice \(label)")
        log.error("error \(3) habs")
        log.fault("fault")
        f.flush()
        XCTAssertEqual(try lines(url).map(\.msg), ["notice <private>", "error 3 habs", "fault"])
        XCTAssertEqual(try lines(url).map(\.lvl), ["notice", "error", "fault"])
    }
}
