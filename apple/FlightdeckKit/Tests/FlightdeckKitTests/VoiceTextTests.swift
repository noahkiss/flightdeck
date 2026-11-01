import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The spoken-text trimmer and the voice grammar (app.md 13.2, 13.3).
final class VoiceTextTests: XCTestCase {
    private func msg(_ id: String, _ kind: ChatKind, _ text: String?, parent: String? = nil, sender: String? = nil) -> ChatMessage {
        ChatMessage(at: "2026-11-01T10:00:00Z", id: id, kind: kind, parent: parent, sender: sender, session: "s1", text: text)
    }

    // MARK: - Trimmer

    func testCodeDiffAndTableBecomePlaceholders() {
        let md = """
        I changed the parser.

        ```go
        func main() {}
        ```

        ```diff
        - old
        + new
        ```

        | File | Lines |
        |------|-------|
        | a.go | 3 |

        Tests pass.
        """
        XCTAssertEqual(SpokenText.speakable(md),
                       "I changed the parser. A code block, shown on screen. A diff, shown on screen. A table. Tests pass.")
    }

    func testRepeatedPlaceholdersCollapse() {
        let md = "```\na\n```\n```\nb\n```\nDone"
        XCTAssertEqual(SpokenText.speakable(md), "A code block, shown on screen. Done.")
    }

    func testUnfencedDiffIsDropped() {
        let md = "Here it is:\ndiff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n\nThat is all."
        XCTAssertEqual(SpokenText.speakable(md), "Here it is: A diff, shown on screen. That is all.")
    }

    func testMarkdownMarksLinksAndPaths() {
        let md = """
        ## Summary
        - **Fixed** the bug in `internal/link/link_test.go`.
        - See [the docs](https://example.com/docs) and https://www.example.org/a/b.
        > Quoted _note_ about snake_case_name.
        1. Run ./scripts/check.sh
        ---
        """
        XCTAssertEqual(SpokenText.speakable(md),
                       "Summary. Fixed the bug in link_test.go. See the docs and example.org. Quoted note about snake_case_name. Run check.sh.")
    }

    func testWordsWithSlashesThatAreNotPaths() {
        XCTAssertEqual(SpokenText.speakable("Use and/or here."), "Use and/or here.")
    }

    func testSummaryLineWins() {
        let md = """
        Long explanation with code:
        ```
        x
        ```
        **Spoken summary:** I fixed the flaky test. It passes twenty times in a row now.
        """
        XCTAssertEqual(SpokenText.speakable(md), "I fixed the flaky test. It passes twenty times in a row now.")
    }

    func testCapSaysTheRestIsInTheChat() {
        let text = (1...30).map { "Sentence number \($0) is here." }.joined(separator: " ")
        let s = SpokenText.speakable(text, capWords: 20)
        XCTAssertTrue(s.hasSuffix("The rest is in the chat."), s)
        XCTAssertEqual(s, "Sentence number 1 is here. Sentence number 2 is here. Sentence number 3 is here. Sentence number 4 is here. The rest is in the chat.")
        // No sentence end in the second half: cut at the word.
        let run = Array(repeating: "word", count: 40).joined(separator: " ")
        XCTAssertEqual(SpokenText.speakable(run, capWords: 3), "word word word. The rest is in the chat.")
    }

    func testFinalTextIsTheLastTopLevelAgentTextOfTheTurn() {
        let ms = [
            msg("1", .user, "first"),
            msg("2", .agent, "old reply"),
            msg("3", .user, "fix it"),
            msg("4", .agent, "Looking."),
            msg("5", .tool, nil),
            msg("6", .agent, "Subagent text", parent: "5"),
            msg("7", .agent, "Fixed it."),
            msg("8", .tool, nil),
            msg("9", .user, "from a channel", sender: "other-agent"),
        ]
        XCTAssertEqual(SpokenText.finalText(ms), "Fixed it.")
        XCTAssertNil(SpokenText.finalText([msg("1", .agent, "x"), msg("2", .user, "y")]))
    }

    func testReplyDoneAndStopped() {
        XCTAssertEqual(SpokenText.reply([msg("1", .user, "go")], stopped: false), "Done.")
        XCTAssertEqual(SpokenText.reply([msg("1", .user, "go"), msg("2", .agent, "Rate limit hit. Try later.")], stopped: true),
                       "The turn stopped. Rate limit hit.")
        XCTAssertEqual(SpokenText.reply([msg("1", .user, "go")], stopped: true), "The turn stopped.")
        XCTAssertEqual(SpokenText.reply([msg("1", .user, "go"), msg("2", .agent, "All *good*")], stopped: false), "All good.")
    }

    func testPromptCarriesTheSummaryRequest() {
        XCTAssertEqual(SpokenText.prompt("run the tests", askSummary: false), "run the tests")
        let p = SpokenText.prompt("run the tests", askSummary: true)
        XCTAssertTrue(p.hasPrefix("run the tests\n\n"))
        XCTAssertTrue(p.contains(SpokenText.summaryMarker))
    }

    // MARK: - Grammar

    func testCommandsMatchOnlyTheWholeUtterance() {
        XCTAssertEqual(VoiceGrammar.command("Stop."), .stop)
        XCTAssertEqual(VoiceGrammar.command("  stop!  "), .stop)
        XCTAssertNil(VoiceGrammar.command("stop the server"))
        XCTAssertEqual(VoiceGrammar.command("Repeat"), .repeatReply)
        XCTAssertEqual(VoiceGrammar.command("approve"), .allow)
        XCTAssertEqual(VoiceGrammar.command("Deny."), .deny)
        XCTAssertEqual(VoiceGrammar.command("End voice"), .endVoice)
        XCTAssertNil(VoiceGrammar.command("please deny"))
    }

    func testAnswersAreAllowOrDenyOnly() {
        XCTAssertEqual(VoiceGrammar.answer("Allow."), .allow)
        XCTAssertEqual(VoiceGrammar.answer("approve"), .allow)
        XCTAssertEqual(VoiceGrammar.answer("Deny"), .deny)
        XCTAssertEqual(VoiceGrammar.answer("no"), .deny)
        XCTAssertNil(VoiceGrammar.answer("yes"))
        XCTAssertNil(VoiceGrammar.answer("allow and then deploy"))
        XCTAssertNil(VoiceGrammar.answer("I think allow"))
        XCTAssertNil(VoiceGrammar.answer(""))
    }

    func testReadBackSpeaksTheCommand() {
        let r = PermissionRequest(command: "wrangler deploy --env prod", id: "r1", title: "Run wrangler deploy?", tool: "Bash")
        XCTAssertEqual(VoiceGrammar.speakCommand("wrangler deploy --env prod"), "wrangler deploy, dash dash env prod")
        XCTAssertEqual(VoiceGrammar.readBack(r, mayAllow: true), "Run wrangler deploy? wrangler deploy, dash dash env prod. Allow or deny?")
        XCTAssertEqual(VoiceGrammar.speakCommand("cd /srv/app && rm -rf build"), "cd app and then rm, dash rf build")
        let untitled = PermissionRequest(command: "ls", id: "r2", tool: "Bash")
        XCTAssertTrue(VoiceGrammar.readBack(untitled, mayAllow: false).hasPrefix("Allow Bash? ls. This one can be allowed only on screen."))
    }

    func testVoiceAllowLimits() {
        let now = Date()
        let short = PermissionRequest(command: "go test ./...", id: "r1")
        XCTAssertTrue(VoiceGrammar.voiceMayAllow(short, now: now))
        XCTAssertFalse(VoiceGrammar.voiceMayAllow(PermissionRequest(command: "go test ./..."), now: now), "no id")
        let long = PermissionRequest(command: String(repeating: "a ", count: 20), id: "r1")
        XCTAssertFalse(VoiceGrammar.voiceMayAllow(long, now: now), "over 16 words")
        let wide = PermissionRequest(command: String(repeating: "x", count: 101), id: "r1")
        XCTAssertFalse(VoiceGrammar.voiceMayAllow(wide, now: now), "over 100 characters")
        let expired = PermissionRequest(command: "ls", expiresAt: "2026-01-01T00:00:00Z", id: "r1")
        XCTAssertFalse(VoiceGrammar.voiceMayAllow(expired, now: now))
        XCTAssertTrue(VoiceGrammar.voiceMayAllow(PermissionRequest(files: ["a.go"], id: "r1"), now: now))
        XCTAssertFalse(VoiceGrammar.voiceMayAllow(PermissionRequest(files: ["a", "b", "c", "d"], id: "r1"), now: now))
        XCTAssertFalse(VoiceGrammar.voiceMayAllow(nil, now: now))
    }
}
