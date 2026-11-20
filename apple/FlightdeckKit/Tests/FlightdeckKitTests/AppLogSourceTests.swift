import Foundation
import XCTest

/// The app's source check (diagnostics.md 4.10, 10.2): every log line goes through `AppLog`,
/// so its values are private by default. Outside `Diagnostics/AppLog.swift` no source may set
/// `privacy:`, make an `os.Logger`, call `os_log`, `NSLog`, `print` or `debugPrint` (except the
/// listed files), or build an `AppLogMessage` from a runtime string. A value passed as
/// `\(id: …)` or `\(word: …)`, which the log keeps public, must be listed for its file.
final class AppLogSourceTests: XCTestCase {
    /// The public values each file may log, as "label: expression". Each is an id Flightdeck
    /// minted, an enum case, an API method name or an error kind. Add one only when it can never
    /// hold a host name, a label, a path or text a user or a hab wrote.
    static let safe: [String: Set<String>] = [
        "Terminal/TerminalModel.swift": ["id: self.pane"],
        "Console/ConsoleModel.swift": ["id: self.pane"],
        "Push/ApprovalCenter.swift": [
            "word: params.choice",
            "word: String(describing: state)", // the locked-signing case
            "word: o.caseName",
        ],
        "Push/PushRegistrar.swift": ["word: self.environment"],
        "SSH/SSHSetup.swift": ["word: k.kind", "word: r.key.type", #"word: r == .added ? "added" : "present""#],
        "RPC/RPCClient.swift": [
            "word: method", // an API method name read from the line, or "unknown"
            "word: Self.kind(of: error)", // a decoding error's kind
            "word: method", // failedCall: an API method name
            "id: tr", // failedCall: a trace in its checked form (diagnostics.md 5.2)
        ],
    ]

    /// Files that may print to stdout: DEBUG-only lines for Xcode's console, with no private
    /// value (a public key line, a duration).
    static let mayPrint: Set<String> = ["SSH/SSHSetup.swift", "SSH/NIOSSHTransport.swift"]

    static let allowedFile = "Diagnostics/AppLog.swift"

    static let bans: [(pattern: String, what: String)] = [
        (#"privacy\s*:"#, "privacy: outside AppLog"),
        (#"\bLogger\s*(\.\s*init\s*)?\("#, "os.Logger outside AppLog"),
        (#"\bos_log\s*\("#, "os_log outside AppLog"),
        (#"\bNSLog\s*\("#, "NSLog"),
        (#"\bdebugPrint\s*\("#, "debugPrint"),
        (#"stringLiteral\s*:"#, "an AppLogMessage built from a runtime string"),
    ]

    static func matches(_ line: String, _ pattern: String) -> Bool {
        line.range(of: pattern, options: .regularExpression) != nil
    }

    /// The problems in one source file. file is its path; the allowlists match its tail.
    static func problems(in text: String, file: String) -> [String] {
        if file.hasSuffix(allowedFile) { return [] }
        let safeHere = safe.first { file.hasSuffix($0.key) }?.value ?? []
        let printOK = mayPrint.contains { file.hasSuffix($0) }
        var out: [String] = []
        for (i, line) in text.components(separatedBy: "\n").enumerated() {
            if line.trimmingCharacters(in: .whitespaces).hasPrefix("//") { continue }
            let at = "\(file):\(i + 1)"
            for b in bans where matches(line, b.pattern) { out.append("\(at): \(b.what)") }
            if !printOK, matches(line, #"(^|[^\w.])print\s*\("#) { out.append("\(at): print") }
            for (label, expr) in labelled(line) where !safeHere.contains("\(label): \(expr)") {
                out.append("\(at): \\(\(label): \(expr)) is not a listed safe value for this file")
            }
        }
        return out
    }

    /// The `\(id: …)` and `\(word: …)` interpolations in a line, raw strings and any spacing
    /// included, as (label, expression).
    static func labelled(_ line: String) -> [(String, String)] {
        let re = try! NSRegularExpression(pattern: ##"\\#*\(\s*(id|word)\s*:\s*"##)
        let ns = line as NSString
        var out: [(String, String)] = []
        for m in re.matches(in: line, range: NSRange(location: 0, length: ns.length)) {
            let label = ns.substring(with: m.range(at: 1))
            let rest = Array(ns.substring(from: m.range.location + m.range.length))
            var depth = 1, inString = false, expr = ""
            for c in rest {
                if c == "\"" { inString.toggle() }
                if !inString {
                    if c == "(" { depth += 1 }
                    if c == ")" { depth -= 1; if depth == 0 { break } }
                }
                expr.append(c)
            }
            out.append((label, expr.trimmingCharacters(in: .whitespaces)))
        }
        return out
    }

    func testCheckerCatchesEachBan() {
        let cases: [(String, String)] = [
            (#"let log = Logger(subsystem: "x", category: "y")"#, "os.Logger"),
            (#"let log = Logger.init(subsystem: "x", category: "y")"#, "os.Logger"),
            (#"log.notice("host \(host, privacy: .public)")"#, "privacy:"),
            (#"os_log("x")"#, "os_log"),
            (#"NSLog("%@", host)"#, "NSLog"),
            (#"debugPrint(host)"#, "debugPrint"),
            (#"print("host: \(host)")"#, "print"),
            (#"log.notice(AppLogMessage(stringLiteral: host))"#, "runtime string"),
            (#"log.notice("pane \(id: hostName)")"#, "(id: hostName)"),
            (#"log.notice("pane \(id:hostName)")"#, "(id: hostName)"),
            (#"log.notice("pane \(  word :  label )")"#, "(word: label)"),
            (##"log.notice(#"pane \#(id: hostName)"#)"##, "(id: hostName)"),
            // Listed for another file only: public there, not here.
            (#"log.notice("pane \(id: self.pane)")"#, "(id: self.pane)"),
        ]
        for (src, want) in cases {
            let p = Self.problems(in: src, file: "FlightdeckKit/Sources/FlightdeckKit/Board/Fixture.swift")
            XCTAssertEqual(p.count, 1, "\(src): \(p)")
            XCTAssertTrue(p.first?.contains(want) == true, "\(src): \(p)")
        }
        let listed = #"log.notice("pane \(id: self.pane): \(kind: error)")"#
        XCTAssertEqual(Self.problems(in: listed, file: "FlightdeckKit/Sources/FlightdeckKit/Terminal/TerminalModel.swift"), [])
        XCTAssertEqual(Self.problems(in: #"print("x")"#, file: "FlightdeckKit/Sources/FlightdeckKit/SSH/SSHSetup.swift"), [])
        XCTAssertEqual(Self.problems(in: #"let p = Logger(subsystem: "x", category: "y")"#,
                                     file: "FlightdeckKit/Sources/FlightdeckKit/" + Self.allowedFile), [])
        XCTAssertEqual(Self.problems(in: "    // print(x) and Logger(...) in a comment", file: "X.swift"), [])
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
