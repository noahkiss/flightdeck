import Foundation
import HabAPI
import SwiftUI
import XCTest
@testable import FlightdeckKit

/// Snapshot tests from the hab's fixture transcripts.
///
/// Input: each `<harness>-<name>.chat.json` in `internal/transcript/testdata`,
/// the chat messages the hab's reader produces from a hand-written transcript.
/// Output: the chat view's rows as plain text (`ChatRender.text`), compared
/// with `Snapshots/<harness>-<name>.txt`. A new harness's fixtures drop in by
/// name. `FLIGHTDECK_UPDATE_SNAPSHOTS=1` rewrites the text files.
///
/// On macOS each fixture also renders through `ImageRenderer`. The image is
/// checked for size only, not compared by pixel, because fonts and system
/// versions change pixels. `FLIGHTDECK_SNAPSHOT_DIR` saves the PNGs for a look.
@MainActor
final class ChatSnapshotTests: XCTestCase {
    static let harnesses = ["claude", "codex", "opencode"]

    static var repo: URL {
        URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent() // FlightdeckKitTests
            .deletingLastPathComponent() // Tests
            .deletingLastPathComponent() // FlightdeckKit
            .deletingLastPathComponent() // apple
            .deletingLastPathComponent()
    }

    static var fixtureDir: URL { repo.appendingPathComponent("internal/transcript/testdata") }
    static var snapshotDir: URL { URL(fileURLWithPath: #filePath).deletingLastPathComponent().appendingPathComponent("Snapshots") }

    /// Fixture files by harness.
    static func fixtures() throws -> [String: [URL]] {
        let files = try FileManager.default.contentsOfDirectory(at: fixtureDir, includingPropertiesForKeys: nil)
        var out: [String: [URL]] = [:]
        for f in files where f.lastPathComponent.hasSuffix(".chat.json") {
            let h = String(f.lastPathComponent.prefix { $0 != "-" })
            out[h, default: []].append(f)
        }
        return out.mapValues { $0.sorted { $0.lastPathComponent < $1.lastPathComponent } }
    }

    static func messages(_ url: URL) throws -> [ChatMessage] {
        try JSONDecoder().decode([ChatMessage].self, from: Data(contentsOf: url))
    }

    func testTextSnapshotsForEachHarnessFixture() throws {
        let all = try Self.fixtures()
        XCTAssertFalse(all["claude", default: []].isEmpty, "the Claude Code fixtures are missing")
        let update = ProcessInfo.processInfo.environment["FLIGHTDECK_UPDATE_SNAPSHOTS"] == "1"
        for h in Self.harnesses where all[h] == nil {
            print("chat snapshots: no \(h) fixtures in internal/transcript/testdata yet")
        }
        for (_, files) in all.sorted(by: { $0.key < $1.key }) {
            for f in files {
                let name = f.lastPathComponent.replacingOccurrences(of: ".chat.json", with: "")
                let text = ChatRender.text(ChatRender.items(try Self.messages(f)))
                let golden = Self.snapshotDir.appendingPathComponent(name + ".txt")
                if update {
                    try FileManager.default.createDirectory(at: Self.snapshotDir, withIntermediateDirectories: true)
                    try text.write(to: golden, atomically: true, encoding: .utf8)
                    continue
                }
                guard let want = try? String(contentsOf: golden, encoding: .utf8) else {
                    XCTFail("no snapshot \(golden.lastPathComponent); run with FLIGHTDECK_UPDATE_SNAPSHOTS=1")
                    continue
                }
                XCTAssertEqual(text, want, "snapshot \(name) differs; run with FLIGHTDECK_UPDATE_SNAPSHOTS=1 if the change is right")
            }
        }
    }

    /// Every message becomes a row; unknown records become placeholders, never errors.
    func testEveryMessageRendersAndUnknownIsPlaceholder() throws {
        for (_, files) in try Self.fixtures() {
            for f in files {
                let ms = try Self.messages(f)
                let items = ChatRender.items(ms)
                XCTAssertEqual(items.count, ms.filter { $0.parent == nil }.count, f.lastPathComponent)
                for m in ms where m.kind == .event && m.event == .unknown {
                    XCTAssertTrue(items.contains { if case .event(m.id, .unknown, _) = $0 { return true }; return false })
                }
            }
        }
        let odd = ChatMessage(at: "2026-01-01T00:00:00Z", id: "x", kind: ChatKind(rawValue: "hologram"), session: "s")
        XCTAssertEqual(ChatRender.items([odd]), [.unsupported(id: "x", kind: "hologram")])
    }

    func testSubagentNestsAndPlanFolds() {
        let call = ChatMessage(at: "t", id: "t1", kind: .tool, session: "s", tool: ChatTool(name: "Task", status: .running, target: "explore"))
        let child = ChatMessage(at: "t", id: "c1", kind: .agent, parent: "t1", session: "s", text: "inside")
        let p1 = ChatMessage(at: "t", id: "p1", kind: .plan, plan: [PlanStep(status: .pending, text: "a")], session: "s")
        let p2 = ChatMessage(at: "t", id: "p2", kind: .plan, plan: [PlanStep(status: .done, text: "a"), PlanStep(status: .inProgress, text: "b")], session: "s")
        let text = ChatRender.text(ChatRender.items([call, child, p1, p2]))
        XCTAssertEqual(text, """
        TOOL Task [running] explore (folded)
            | AGENT
            |     inside
        PLAN 0 of 1 (earlier, folded)
        PLAN 1 of 2
            [x] a
            [>] b

        """)
    }

    #if os(macOS)
    func testImageRenderOfEachFixture() throws {
        let dir = ProcessInfo.processInfo.environment["FLIGHTDECK_SNAPSHOT_DIR"].map { URL(fileURLWithPath: $0) }
        for (_, files) in try Self.fixtures() {
            for f in files {
                let items = ChatRender.items(try Self.messages(f))
                let view = VStack(alignment: .leading, spacing: 12) {
                    ForEach(items) { ChatItemView(item: $0) }
                }
                .padding(12)
                .frame(width: 420)
                .background(Color.white)
                .environment(\.colorScheme, .light)
                let r = ImageRenderer(content: view)
                r.scale = 2
                let img = try XCTUnwrap(r.cgImage, f.lastPathComponent)
                XCTAssertEqual(img.width, 840)
                XCTAssertGreaterThan(img.height, items.count * 20, "each row has height")
                if let dir, let tiff = r.nsImage?.tiffRepresentation, let rep = NSBitmapImageRep(data: tiff),
                   let png = rep.representation(using: .png, properties: [:]) {
                    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
                    try png.write(to: dir.appendingPathComponent(f.lastPathComponent.replacingOccurrences(of: ".chat.json", with: ".png")))
                }
            }
        }
    }
    #endif
}
