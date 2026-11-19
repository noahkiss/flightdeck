import Foundation
import XCTest

/// The app's source check (diagnostics.md 4.10, 10.2): every log line goes through `AppLog`,
/// so its values are private by default. Outside `Diagnostics/AppLog.swift` no source may set
/// `privacy:`, make an `os.Logger` or call `os_log`, and a value passed as `\(id: …)` or
/// `\(word: …)`, which the log keeps public, must be one of the listed safe expressions.
final class AppLogSourceTests: XCTestCase {
    /// The expressions that may pass as public values. Each is an id Flightdeck minted, an enum
    /// case, an API method name or an error kind. Add one only when it can never hold a host
    /// name, a label, a path or text a user or a hab wrote.
    static let safe: [String: Set<String>] = [
        "id": ["self.pane"],
        "word": [
            "params.choice",
            "String(describing: state)", // ApprovalCenter: the locked-signing case
            "o.caseName",
            "self.environment",
            "method", // RPCClient: an API method name, or "unknown"
            "Self.kind(of: error)",
            "k.kind",
            "r.key.type",
            #"r == .added ? "added" : "present""#,
        ],
    ]

    static let allowedFile = "Diagnostics/AppLog.swift"

    /// The problems in one source file.
    static func problems(in text: String, file: String) -> [String] {
        if file.hasSuffix(allowedFile) { return [] }
        var out: [String] = []
        for (i, line) in text.components(separatedBy: "\n").enumerated() {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("//") || trimmed.hasPrefix("///") { continue }
            let at = "\(file):\(i + 1)"
            if line.contains("privacy:") { out.append("\(at): privacy: outside AppLog") }
            if line.range(of: #"\bLogger\("#, options: .regularExpression) != nil { out.append("\(at): os.Logger outside AppLog") }
            if line.contains("os_log(") { out.append("\(at): os_log outside AppLog") }
            for (label, expr) in labelled(line) where !(safe[label]?.contains(expr) ?? false) {
                out.append("\(at): \\(\(label): \(expr)) is not a listed safe value")
            }
        }
        return out
    }

    /// The `\(id: …)` and `\(word: …)` interpolations in a line, as (label, expression).
    static func labelled(_ line: String) -> [(String, String)] {
        var out: [(String, String)] = []
        let chars = Array(line)
        var i = 0
        while i + 1 < chars.count {
            guard chars[i] == "\\", chars[i + 1] == "(" else { i += 1; continue }
            let rest = String(chars[(i + 2)...])
            guard let label = ["id", "word"].first(where: { rest.hasPrefix($0 + ": ") }) else { i += 2; continue }
            var j = i + 2 + label.count + 2, depth = 1, inString = false
            var expr = ""
            while j < chars.count {
                let c = chars[j]
                if c == "\"" { inString.toggle() }
                if !inString {
                    if c == "(" { depth += 1 }
                    if c == ")" { depth -= 1; if depth == 0 { break } }
                }
                expr.append(c)
                j += 1
            }
            out.append((label, expr))
            i = j + 1
        }
        return out
    }

    func testCheckerCatchesEachBan() {
        let fixture = #"""
        let log = Logger(subsystem: "x", category: "y")
        log.notice("host \(host, privacy: .public)")
        os_log("x")
        log.notice("pane \(id: hostName) via \(word: label)")
        log.notice("pane \(id: self.pane): \(word: Self.kind(of: error))")
        """#
        let p = Self.problems(in: fixture, file: "Fixture.swift")
        XCTAssertEqual(p.count, 5, p.joined(separator: "\n"))
        XCTAssertTrue(p.contains { $0.contains("Logger") })
        XCTAssertTrue(p.contains { $0.contains("privacy:") })
        XCTAssertTrue(p.contains { $0.contains("os_log") })
        XCTAssertTrue(p.contains { $0.contains("(id: hostName)") })
        XCTAssertTrue(p.contains { $0.contains("(word: label)") })
        XCTAssertEqual(Self.problems(in: fixture, file: "Sources/FlightdeckKit/" + Self.allowedFile), [])
    }

    func testAppSourcesPass() throws {
        let apple = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent()
        let roots = ["FlightdeckKit/Sources", "App", "NotificationService", "Widgets", "Shared"]
        var files = 0
        var problems: [String] = []
        for root in roots {
            let dir = apple.appendingPathComponent(root)
            guard let e = FileManager.default.enumerator(at: dir, includingPropertiesForKeys: nil) else { continue }
            for case let url as URL in e where url.pathExtension == "swift" {
                let text = try String(contentsOf: url, encoding: .utf8)
                let rel = String(url.path.dropFirst(apple.path.count + 1))
                problems += Self.problems(in: text, file: rel)
                files += 1
            }
        }
        XCTAssertGreaterThan(files, 50, "the check found too few sources: wrong folder?")
        XCTAssertEqual(problems, [], problems.joined(separator: "\n"))
    }
}
