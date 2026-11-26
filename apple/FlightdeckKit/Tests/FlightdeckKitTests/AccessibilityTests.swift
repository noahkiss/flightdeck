import Foundation
import HabAPI
import SwiftUI
import XCTest
@testable import FlightdeckKit

/// Accessibility and localisation (app.md 22): the chat's rotors, the state shapes, the
/// high-contrast diff colours, Reduce Motion and counts in words.
final class AccessibilityTests: XCTestCase {
    private func tool(_ id: String, _ name: String, _ verb: String, subject: String? = nil, kind: ChatContentKind = .commands) -> ChatItem {
        .tool(id: id, card: ToolCard(name: name, target: nil, status: .done, sentence: ToolSentence(verb: verb, subject: subject),
                                     kind: kind, oneLine: false, detail: .none, fields: [], output: nil,
                                     outputTruncated: false, children: []))
    }

    // MARK: - Rotors (22.1)

    func testRotorsListTurnsToolsAndRequests() {
        let items: [ChatItem] = [
            .user(id: "u1", text: "Fix the failing test\nand push", sender: nil, attachments: []),
            .agent(id: "a1", text: "On it.", summary: nil, sender: nil),
            tool("t1", "Bash", "Ran", subject: "go test ./..."),
            .event(id: "e1", event: .permission, text: "Allowed Bash: rm -rf build"),
            .event(id: "e2", event: .compaction, text: "Compacted"),
            tool("t2", "AskUserQuestion", "Asked"),
            .user(id: "u2", text: "", sender: nil, attachments: ["x", "y"]),
            .user(id: "u3", text: "Looks good", sender: "reviewer", attachments: []),
        ]
        let r = ChatRotors.of(items.map(ChatRow.item))
        XCTAssertEqual(r.turns, [
            ChatRotorEntry(id: "u1", label: "You: Fix the failing test"),
            ChatRotorEntry(id: "u2", label: "You: 2 attachments"),
            ChatRotorEntry(id: "u3", label: "From reviewer: Looks good"),
        ])
        XCTAssertEqual(r.tools.map(\.id), ["t1", "t2"])
        XCTAssertEqual(r.tools[0].label, "Ran go test ./...")
        XCTAssertEqual(r.requests.map(\.id), ["e1", "t2"])
        XCTAssertEqual(r.requests[0].label, "Allowed Bash: rm -rf build")
    }

    /// A folded line stands for the tool calls inside it: their rows have no view until it opens.
    func testRotorsPointAtAFoldNotItsHiddenRows() {
        let items: [ChatItem] = [
            .user(id: "u1", text: "Look around", sender: nil, attachments: []),
            tool("t1", "Read", "Read", subject: "a.swift", kind: .reads),
            tool("t2", "Read", "Read", subject: "b.swift", kind: .reads),
            .agent(id: "a1", text: "Done.", summary: nil, sender: nil),
        ]
        let rows = ChatFold.rows(items, hidden: [.reads])
        let r = ChatRotors.of(rows)
        XCTAssertEqual(r.tools.map(\.id), ["fold-t1"])
        XCTAssertEqual(r.tools[0].label, "2 steps hidden: 2 reads")
        // Every entry names a row the transcript draws.
        let ids = Set(rows.map(\.id))
        XCTAssertTrue((r.turns + r.tools + r.requests).allSatisfy { ids.contains($0.id) })
    }

    /// The Capcom thread has the same rotors: the owner's messages, each turn's tool line, and
    /// the cards that ask the owner. Every entry names a row the thread draws.
    func testCapcomThreadRotors() {
        func card(_ id: String, _ type: String, _ text: String?, choices: [String] = ["allow", "deny"], grant: CapcomCardGrant? = nil) -> CapcomRow {
            .card(CapcomItem(author: CapcomAuthor(kind: .capcom), card: CapcomCard(choices: choices, grant: grant, type: type),
                             id: id, kind: CapcomItemKind(rawValue: "card"), text: text, time: "2026-11-01T12:00:00Z"))
        }
        let rows: [CapcomRow] = [
            .user(id: "u1", text: "What broke the build?\nThanks", attachments: [], context: [], read: true),
            .capcom(id: "c1", text: "Checking.", wroteFirst: false),
            .tools(id: "t1", line: "read 2 transcripts, searched the web", calls: []),
            card("k1", "approval", "Fetch example.com?"),
            card("k2", "calendar", "Dentist on Friday", choices: ["add"]),
            card("k3", "grant", nil),
            card("k4", "reminder", "Water the plants", choices: []),
            card("k5", "memory", "Remembered: tea, no sugar", choices: ["undo"]),
            card("k6", "grant", nil, choices: ["grant"], grant: CapcomCardGrant(actions: [], name: "Morning mail", prompt: "", revision: 1, round: "r1", trigger: "")),
            .note(id: "n1", text: "Capcom restarted"),
            .user(id: "u2", text: "", attachments: [], context: [CapcomContextItem(host: "h", kind: .session, ref: "s1", title: "fix-login")], read: false),
            .user(id: "u3", text: "", attachments: ["a"], context: [], read: false),
        ]
        let r = ChatRotors.of(rows)
        XCTAssertEqual(r.turns, [
            ChatRotorEntry(id: "u1", label: "You: What broke the build?"),
            ChatRotorEntry(id: "u2", label: "You: fix-login"),
            ChatRotorEntry(id: "u3", label: "You: 1 attachment"),
        ])
        XCTAssertEqual(r.tools, [ChatRotorEntry(id: "t1", label: "read 2 transcripts, searched the web")])
        XCTAssertEqual(r.requests, [ChatRotorEntry(id: "k1", label: "Fetch example.com?"), ChatRotorEntry(id: "k2", label: "Dentist on Friday"),
                                    ChatRotorEntry(id: "k3", label: "Capcom asks"), ChatRotorEntry(id: "k6", label: "Morning mail")])
        let ids = Set(rows.map(\.id))
        XCTAssertTrue((r.turns + r.tools + r.requests).allSatisfy { ids.contains($0.id) })
    }

    func testFirstLineSkipsEmptyLines() {
        XCTAssertEqual(ToolSentence.firstLine("one\ntwo"), "one")
        XCTAssertEqual(ToolSentence.firstLine("\n\nthree\nfour"), "three")
        XCTAssertEqual(ToolSentence.firstLine("only"), "only")
        XCTAssertEqual(ToolSentence.firstLine("\n\nlast"), "last")
        XCTAssertEqual(ToolSentence.firstLine("\n\n"), "\n\n")
        XCTAssertEqual(ToolSentence.firstLine(""), "")
    }

    func testRotorLabelsAreCut() {
        let long = String(repeating: "word ", count: 40)
        let r = ChatRotors.of([.item(.user(id: "u", text: long, sender: nil, attachments: []))])
        XCTAssertEqual(r.turns[0].label.count, "You: ".count + ChatRotors.labelMax)
        XCTAssertTrue(r.turns[0].label.hasSuffix("…"))
    }

    // MARK: - State shapes (22.2)

    func testEachStateHasItsOwnShape() {
        let cases: [(AgentStatus?, BoardRank, StateDotShape)] = [
            (.blocked, .blocked, .waiting),
            (.error, .error, .error),
            (.done, .doneUnseen, .doneUnseen),
            (.done, .atRest, .done),
            (.working, .working, .working),
            (.idle, .atRest, .idle),
            (nil, .other, .other),
            (.exited, .other, .other),
        ]
        for (state, rank, shape) in cases {
            XCTAssertEqual(boardRow("r", state: state, rank: rank).dotShape, shape, "\(String(describing: state))")
        }
        let symbols = StateDotShape.allCases.map(\.symbol)
        XCTAssertEqual(Set(symbols).count, symbols.count, "no two states share a symbol")
    }

    // MARK: - Contrast (22.2)

    private func luminance(_ hex: UInt32) -> Double {
        func channel(_ v: UInt32) -> Double {
            let c = Double(v & 0xFF) / 255
            return c <= 0.03928 ? c / 12.92 : pow((c + 0.055) / 1.055, 2.4)
        }
        return 0.2126 * channel(hex >> 16) + 0.7152 * channel(hex >> 8) + 0.0722 * channel(hex)
    }

    private func ratio(_ a: UInt32, _ b: UInt32) -> Double {
        let (l1, l2) = (luminance(a), luminance(b))
        return (max(l1, l2) + 0.05) / (min(l1, l2) + 0.05)
    }

    /// Diff text passes 4.5:1 on its line, and 7:1 under Increase Contrast; counts and the
    /// gutter the same on the system background (white or black).
    func testDiffPairsPassTheirRatio() {
        for scheme in [ColorScheme.light, .dark] {
            for contrast in [ColorSchemeContrast.standard, .increased] {
                let p = DiffPalette(scheme, contrast)
                let need = contrast == .increased ? 7.0 : 4.5
                let page: UInt32 = scheme == .dark ? 0x000000 : 0xFFFFFF
                let name = "\(scheme) \(contrast)"
                XCTAssertGreaterThanOrEqual(ratio(p.removedTextHex, p.removedBackgroundHex), need, "removed, \(name)")
                XCTAssertGreaterThanOrEqual(ratio(p.addedTextHex, p.addedBackgroundHex), need, "added, \(name)")
                XCTAssertGreaterThanOrEqual(ratio(p.gutterHex, p.removedBackgroundHex), need, "gutter on removed, \(name)")
                XCTAssertGreaterThanOrEqual(ratio(p.gutterHex, p.addedBackgroundHex), need, "gutter on added, \(name)")
                XCTAssertGreaterThanOrEqual(ratio(p.addedCountHex, page), need, "added count, \(name)")
                XCTAssertGreaterThanOrEqual(ratio(p.removedCountHex, page), need, "removed count, \(name)")
            }
        }
    }

    // MARK: - Reduce Motion (22.4)

    func testReduceMotionDropsTheAnimation() {
        XCTAssertNil(gateMotion(.easeOut(duration: 0.15), reduce: true))
        XCTAssertNotNil(gateMotion(.easeOut(duration: 0.15), reduce: false))
    }

    // MARK: - Counts in words (22.3)

    func testCountsAgreeWithTheirNoun() {
        XCTAssertEqual(ChatContentKind.reads.count(1), "1 read")
        XCTAssertEqual(ChatContentKind.reads.count(3), "3 reads")
        XCTAssertEqual(ChatContentKind.plans.count(1), "1 plan update")
        XCTAssertEqual(ChatContentKind.plans.count(2), "2 plan updates")
        XCTAssertEqual(ChatContentKind.connectors.count(2), "2 connector calls")
        XCTAssertEqual(ChatContentKind.agents.count(2), "2 agent messages")
        XCTAssertEqual(ChatContentKind.thinking.count(2), "2 thoughts")
        XCTAssertEqual(ToolSentence.files(1), "1 file")
        XCTAssertEqual(ToolSentence.files(4), "4 files")
        XCTAssertEqual(ToolSentence.searchCount("a.swift:1:x"), "1 match in 1 file")
        XCTAssertEqual(ToolSentence.searchCount("a.swift:1:x\nb.swift:2:y\nb.swift:3:z"), "3 matches in 2 files")
        XCTAssertEqual(ToolSentence.searchCount("a.swift"), "in 1 file")
        XCTAssertEqual(ModifiedFilesCard.title(1), "1 file changed in this session")
        XCTAssertEqual(SettingsWrites.levelNotice(.safe, changed: 1, total: 1, sentences: []), "Safe: 1 setting changed")
        XCTAssertEqual(SettingsWrites.levelNotice(.safe, changed: 2, total: 2, sentences: []), "Safe: 2 settings changed")
    }

    /// A count whose verb or whole phrase changes comes from the catalog's plural variations.
    func testCatalogPluralVariations() {
        XCTAssertEqual(MenuBarNeeds.confirmationsLine(1), "1 confirmation waits")
        XCTAssertEqual(MenuBarNeeds.confirmationsLine(2), "2 confirmations wait")
        XCTAssertEqual(localized("Every \(1) minutes"), "Every minute")
        XCTAssertEqual(localized("Every \(5) minutes"), "Every 5 minutes")
        XCTAssertEqual(localized("Every \(1) hours"), "Every hour")
        XCTAssertEqual(localized("\(1) settings differ from this level."), "1 setting differs from this level.")
        XCTAssertEqual(localized("\(1) reports wait for the coordinator"), "1 report waits for the coordinator")
        XCTAssertEqual(localized("\(3) reports wait for the coordinator"), "3 reports wait for the coordinator")
    }
}
