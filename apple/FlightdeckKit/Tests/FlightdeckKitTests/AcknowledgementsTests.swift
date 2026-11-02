import Foundation
import XCTest
@testable import FlightdeckKit

/// The Acknowledgements page: the split of the notices file, and the file the
/// app bundles (apple/App/Acknowledgements.txt).
final class AcknowledgementsTests: XCTestCase {
    func testSplitsAtEntriesAndFiles() {
        let rule = String(repeating: "=", count: 78)
        let text = "Header\n\nContents\n\n  a\n\n\(rule)\na\nVersion: 1\n\n--- x/LICENSE\n\nMIT\n\n--- x/NOTICE\n(The same text as y above.)\n\n\(rule)\nb\n"
        XCTAssertEqual(AcknowledgementsView.sections(of: text), [
            "Header\n\nContents\n\n  a",
            "a\nVersion: 1",
            "--- x/LICENSE\n\nMIT",
            "--- x/NOTICE\n(The same text as y above.)",
            "b",
        ])
        XCTAssertEqual(AcknowledgementsView.sections(of: ""), [])
    }

    func testBundledFileNamesEveryPinnedPackage() throws {
        let apple = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent()
        let text = try String(contentsOf: apple.appendingPathComponent("App/Acknowledgements.txt"), encoding: .utf8)
        let sections = AcknowledgementsView.sections(of: text)
        let resolved = try Data(contentsOf: apple.appendingPathComponent("FlightdeckKit/Package.resolved"))
        struct Pins: Decodable { struct Pin: Decodable { let identity: String }; let pins: [Pin] }
        let pins = try JSONDecoder().decode(Pins.self, from: resolved).pins
        XCTAssertFalse(pins.isEmpty)
        for pin in pins {
            XCTAssertTrue(sections.contains { $0.hasPrefix(pin.identity + "\nVersion: ") },
                          "\(pin.identity) has no entry; run make notices")
        }
    }
}
