import Foundation
import XCTest
@testable import HabAPI

/// The block vocabulary on the Swift side (docs/specs/modules.md 6, 9). The fixtures are the
/// ones the Go tests check against the schema, under api/modules/testdata/blocks. The app must
/// read every tree a module may send: each valid and beyond-schema tree decodes and survives a
/// round trip; a tree with a block from a later vocabulary decodes, with that block as
/// `.unknown` and its fallback kept.
final class BlockTests: XCTestCase {
    static let fixtureRoot = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent() // HabAPITests
        .deletingLastPathComponent() // Tests
        .deletingLastPathComponent() // HabAPI
        .deletingLastPathComponent() // swift
        .deletingLastPathComponent() // repo root
        .appendingPathComponent("api/modules/testdata/blocks")

    func files(_ folder: String) throws -> [URL] {
        let dir = Self.fixtureRoot.appendingPathComponent(folder)
        let names = try FileManager.default.contentsOfDirectory(atPath: dir.path)
        return names.filter { $0.hasSuffix(".json") }.sorted().map { dir.appendingPathComponent($0) }
    }

    func testFixturesDecodeAndRoundTrip() throws {
        var checked = 0
        for folder in ["valid", "beyond-schema"] {
            for url in try files(folder) {
                let data = try Data(contentsOf: url)
                let block: Block
                do {
                    block = try JSONDecoder().decode(Block.self, from: data)
                } catch {
                    XCTFail("\(folder)/\(url.lastPathComponent): \(error)")
                    continue
                }
                let again = try JSONEncoder().encode(block)
                let want = try JSONSerialization.jsonObject(with: data) as? NSDictionary
                let got = try JSONSerialization.jsonObject(with: again) as? NSDictionary
                XCTAssertEqual(want, got, "\(folder)/\(url.lastPathComponent): round trip changed it")
                checked += 1
            }
        }
        XCTAssertGreaterThanOrEqual(checked, 10)
    }

    func testNewerBlockDecodesAsUnknown() throws {
        let data = try Data(contentsOf: Self.fixtureRoot.appendingPathComponent("newer/terminal.json"))
        let block = try JSONDecoder().decode(Block.self, from: data)
        guard case .stack(let stack) = block.content else {
            return XCTFail("root is \(block.type)")
        }
        XCTAssertEqual(stack.children.count, 2)
        let term = stack.children[1]
        XCTAssertEqual(term.content, .unknown(type: "terminal"))
        XCTAssertEqual(term.type, "terminal")
        XCTAssertEqual(term.id, "term")
        XCTAssertEqual(term.fallback, "Open the pane to see it")
    }

    func testTypedFields() throws {
        let data = try Data(contentsOf: Self.fixtureRoot.appendingPathComponent("valid/controls.json"))
        let block = try JSONDecoder().decode(Block.self, from: data)
        guard case .stack(let root) = block.content,
              case .toggle(let toggle) = root.children[0].content,
              case .stack(let buttons) = root.children[2].content,
              case .button(let clear) = buttons.children[1].content
        else {
            return XCTFail("controls.json has another shape")
        }
        XCTAssertEqual(root.gap, 2)
        XCTAssertTrue(toggle.value)
        XCTAssertEqual(toggle.action.name, "set_watch")
        XCTAssertEqual(clear.role, .destructive)
        XCTAssertEqual(clear.confirm, "Clear every note?")
        XCTAssertEqual(buttons.axis, .horizontal)
    }

    /// An unknown word decodes, so a newer word in a known field does not break an older app.
    func testUnknownWordDecodes() throws {
        let json = #"{"type":"text","text":"x","style":"headline","later_field":1}"#
        let block = try JSONDecoder().decode(Block.self, from: Data(json.utf8))
        guard case .text(let text) = block.content else { return XCTFail("not text") }
        XCTAssertEqual(text.style?.rawValue, "headline")
    }

    /// A known block that breaks a required field fails to decode: the hab never sends one.
    func testBrokenKnownBlocksFail() throws {
        for name in ["missing-title", "button-no-action", "toggle-value-string", "no-type", "root-not-object"] {
            let data = try Data(contentsOf: Self.fixtureRoot.appendingPathComponent("invalid/\(name).json"))
            XCTAssertThrowsError(try JSONDecoder().decode(Block.self, from: data), name)
        }
    }

    func testVocabularyConstants() {
        XCTAssertEqual(BlockVocabulary.version, 1)
        XCTAssertEqual(BlockVocabulary.types.count, 13)
        XCTAssertTrue(BlockVocabulary.tints.contains("blocked"))
    }
}
