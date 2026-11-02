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

    /// A tree with a block of a later vocabulary encodes back unchanged: the unknown block keeps
    /// its fields.
    func testNewerTreeRoundTrips() throws {
        let data = try Data(contentsOf: Self.fixtureRoot.appendingPathComponent("newer/terminal.json"))
        let again = try JSONEncoder().encode(try JSONDecoder().decode(Block.self, from: data))
        let want = try JSONSerialization.jsonObject(with: data) as? NSDictionary
        let got = try JSONSerialization.jsonObject(with: again) as? NSDictionary
        XCTAssertEqual(want, got)
    }

    /// Every invalid fixture the hab would refuse only for strictness (an unknown field, word or
    /// child type, a value out of range, text too long) still decodes: an older app reads a newer
    /// tree. Only a broken required field or a wrong JSON type throws.
    func testInvalidFixturesDecodeLeniently() throws {
        let throwing: Set<String> = [
            "action-args-not-object", "action-no-name", "button-no-action", "children-not-array",
            "gap-not-whole", "log-line-not-string", "missing-title", "no-type", "root-not-object",
            "toggle-value-string", "type-not-string",
        ]
        var lenient = 0
        for url in try files("invalid") {
            let name = url.deletingPathExtension().lastPathComponent
            let data = try Data(contentsOf: url)
            if throwing.contains(name) {
                XCTAssertThrowsError(try JSONDecoder().decode(Block.self, from: data), name)
            } else {
                XCTAssertNoThrow(try JSONDecoder().decode(Block.self, from: data), name)
                lenient += 1
            }
        }
        XCTAssertGreaterThanOrEqual(lenient, 25)
    }

    /// The drawn accessors (docs/specs/modules.md 6.3, 6.4): an unknown or absent word draws as
    /// the field's first word; an unknown tint meaning draws as no tint.
    func testDrawnWords() {
        XCTAssertEqual(StackBlock(children: []).drawnAxis, .vertical)
        XCTAssertEqual(StackBlock(axis: .horizontal, children: []).drawnAxis, .horizontal)
        XCTAssertEqual(StackBlock(axis: BlockStackAxis(rawValue: "diagonal"), children: []).drawnAxis, .vertical)
        XCTAssertEqual(TextBlock(text: "x", style: BlockTextStyle(rawValue: "headline")).drawnStyle, .body)
        XCTAssertEqual(BadgeBlock(text: "x", tint: .blocked).drawnTint, .blocked)
        XCTAssertEqual(BadgeBlock(text: "x", tint: BlockTint(rawValue: "#a0B1c2")).drawnTint?.rawValue, "#a0B1c2")
        XCTAssertNil(BadgeBlock(text: "x", tint: BlockTint(rawValue: "sparkly")).drawnTint)
        XCTAssertNil(BadgeBlock(text: "x", tint: BlockTint(rawValue: "#12345g")).drawnTint)
        XCTAssertNil(BadgeBlock(text: "x").drawnTint)
    }

    static func nested(_ depth: Int, leaf: String = #"{"type":"text","text":"x"}"#) -> Data {
        let open = String(repeating: #"{"type":"stack","children":["#, count: depth - 1)
        let close = String(repeating: "]}", count: depth - 1)
        return Data((open + leaf + close).utf8)
    }

    /// Decodes on a thread with a 512 KB stack, the size GCD and Swift concurrency threads get.
    func decodeOnSmallStack(_ data: Data) -> Result<Block, Error> {
        final class Box: @unchecked Sendable { var result: Result<Block, Error>? }
        let box = Box()
        let done = expectation(description: "decode")
        let thread = Thread {
            box.result = Result { try JSONDecoder().decode(Block.self, from: data) }
            done.fulfill()
        }
        thread.stackSize = 512 * 1024
        thread.start()
        wait(for: [done], timeout: 60)
        return box.result ?? .failure(CocoaError(.featureUnsupported))
    }

    /// A tree past the depth limit throws instead of exhausting the stack; one at the limit
    /// decodes. Free JSON in action args and in an unknown block is guarded the same way.
    func testDepthGuard() throws {
        if case .failure(let error) = decodeOnSmallStack(Self.nested(BlockVocabulary.maxDepth)) {
            XCTFail("a tree at the depth limit: \(error)")
        }
        guard case .failure = decodeOnSmallStack(Self.nested(100)) else {
            return XCTFail("a 100-deep tree decoded")
        }
        let deepJSON = String(repeating: "[", count: 200) + String(repeating: "]", count: 200)
        let args = #"{"type":"button","title":"x","action":{"name":"a","args":{"k":"# + deepJSON + "}}}"
        guard case .failure = decodeOnSmallStack(Data(args.utf8)) else {
            return XCTFail("200-deep action args decoded")
        }
        let unknown = #"{"type":"later","k":"# + deepJSON + "}"
        guard case .failure = decodeOnSmallStack(Data(unknown.utf8)) else {
            return XCTFail("an unknown block with 200-deep JSON decoded")
        }
    }

    func testVocabularyConstants() {
        XCTAssertEqual(BlockVocabulary.version, 1)
        XCTAssertEqual(BlockVocabulary.types.count, 13)
        XCTAssertTrue(BlockVocabulary.tints.contains("blocked"))
    }
}
