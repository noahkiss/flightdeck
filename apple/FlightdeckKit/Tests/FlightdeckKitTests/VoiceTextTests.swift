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

    func testTrimmerEdges() {
        // A table without outer pipes: its header row is taken back.
        XCTAssertEqual(SpokenText.speakable("Results:\nName | Count\n--- | ---\na | 1\nDone."), "Results: A table. Done.")
        // An indented code block after a blank line; a list's continuation is prose.
        XCTAssertEqual(SpokenText.speakable("Run this:\n\n    make test\n    make vet\n\nThen push."), "Run this: A code block, shown on screen. Then push.")
        XCTAssertEqual(SpokenText.speakable("- first\n    more of first"), "first. more of first.")
        // A four-backtick fence holds a triple-backtick line.
        XCTAssertEqual(SpokenText.speakable("````md\n```go\nx\n```\n````\nAfter."), "A code block, shown on screen. After.")
    }

    func testStoppedTurnReadsTheFirstSentence() {
        let ms = [msg("1", .user, "go"), msg("2", .agent, "Build failed in main.go at line 3. Try again.")]
        XCTAssertEqual(SpokenText.reply(ms, stopped: true), "The turn stopped. Build failed in main.go at line 3.")
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

    /// The chat view folds the closing summary line into a caption; the body
    /// keeps everything else, and the speech still reads the summary.
    func testChatFoldsTheSummaryLine() {
        let md = """
        All pass.
        ```
        ok link 0.3s
        ```
        **Spoken summary:** The link tests pass. Nothing else changed.
        """
        let split = SpokenText.splittingSummary(md)
        XCTAssertEqual(split.body, "All pass.\n```\nok link 0.3s\n```")
        XCTAssertEqual(split.summary, "The link tests pass. Nothing else changed.")
        let items = ChatRender.items([msg("a", .agent, md)])
        guard case .agent(_, let blocks, _)? = items.first else { return XCTFail("no agent row") }
        XCTAssertEqual(blocks.last, .spokenSummary("The link tests pass. Nothing else changed."))
        XCTAssertFalse(blocks.contains(.paragraph("**Spoken summary:** The link tests pass. Nothing else changed.")))
        XCTAssertEqual(SpokenText.speakable(md), "The link tests pass. Nothing else changed.")

        // No marker, an empty marker, or code after the marker: the text stays whole.
        XCTAssertNil(SpokenText.splittingSummary("Just an answer.").summary)
        XCTAssertNil(SpokenText.splittingSummary("Answer.\nSpoken summary:").summary)
        XCTAssertNil(SpokenText.splittingSummary("Spoken summary: see\n```\nx\n```").summary)
    }

    /// Automatic picks premium, then enhanced, then default, the exact region
    /// first; a chosen voice wins while it is installed (app.md 13.7).
    func testBestVoicePrefersQuality() {
        typealias V = SpeechVoices.Voice
        let compact = V(id: "c", name: "Samantha", language: "en-US", quality: 1)
        let enhancedGB = V(id: "eg", name: "Daniel", language: "en-GB", quality: 2)
        let enhancedUS = V(id: "eu", name: "Ava", language: "en-US", quality: 2)
        let premiumAU = V(id: "pa", name: "Lee", language: "en-AU", quality: 3)
        let french = V(id: "f", name: "Thomas", language: "fr-FR", quality: 3)
        XCTAssertEqual(SpeechVoices.best([compact, enhancedGB, enhancedUS, french], language: "en-US"), enhancedUS)
        XCTAssertEqual(SpeechVoices.best([compact, enhancedGB, premiumAU], language: "en-US"), premiumAU, "quality over region")
        XCTAssertEqual(SpeechVoices.best([compact], language: "en-US"), compact)
        XCTAssertNil(SpeechVoices.best([french], language: "en-US"))
        XCTAssertEqual(SpeechVoices.candidates([compact, french, enhancedGB], language: "en_US").map(\.id), ["eg", "c"])
        XCTAssertEqual(SpeechVoices.pick([compact, enhancedUS], chosen: "c", language: "en-US"), compact)
        XCTAssertEqual(SpeechVoices.pick([compact, enhancedUS], chosen: "gone", language: "en-US"), enhancedUS, "an uninstalled choice falls back")
        XCTAssertEqual(premiumAU.qualityText, "Premium")

        // The setting round-trips, and older saved settings read as automatic.
        var s = VoiceSettings()
        s.speechVoice = "eu"
        let back = try? JSONDecoder().decode(VoiceSettings.self, from: JSONEncoder().encode(s))
        XCTAssertEqual(back?.speechVoice, "eu")
        XCTAssertNil((try? JSONDecoder().decode(VoiceSettings.self, from: Data("{}".utf8)))?.speechVoice)
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

    func testTheChatHidesTheSummaryRequest() {
        let sent = SpokenText.prompt("run the tests", askSummary: true)
        XCTAssertEqual(SpokenText.hidingSummaryRequest(sent), "run the tests")
        XCTAssertEqual(SpokenText.hidingSummaryRequest("typed text"), "typed text")
        let items = ChatRender.items([msg("1", .user, sent)])
        guard case .user(_, let text, _, _)? = items.first else { return XCTFail("no user row") }
        XCTAssertEqual(text, "run the tests")
    }

    func testPromptCarriesTheSummaryRequest() {
        XCTAssertEqual(SpokenText.prompt("run the tests", askSummary: false), "run the tests")
        let p = SpokenText.prompt("run the tests", askSummary: true)
        XCTAssertTrue(p.hasPrefix("run the tests\n\n"))
        XCTAssertTrue(p.contains(SpokenText.summaryMarker))
    }

    // MARK: - Dictation text

    func testDictationJoinsTheDraftAndStripsControlsForAPane() {
        XCTAssertEqual(ComposerView.join("", "hello"), "hello")
        XCTAssertEqual(ComposerView.join("fix", "the test"), "fix the test")
        XCTAssertEqual(ComposerView.join("fix ", "the test"), "fix the test")
        XCTAssertEqual(ComposerView.join("fix", ""), "fix")
        XCTAssertEqual(String(decoding: PaneDictationButton.paneText("ls\n-la\u{1b}[A\r"), as: UTF8.self), "ls -la [A ")
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
        XCTAssertEqual(VoiceGrammar.answer("Yes."), .allow, "after a read-back, yes is an Allow")
        XCTAssertNil(VoiceGrammar.command("yes"), "a lone yes in an ordinary turn is a message")
        XCTAssertNil(VoiceGrammar.answer("yes please run it"))
        XCTAssertNil(VoiceGrammar.answer("allow and then deploy"))
        XCTAssertNil(VoiceGrammar.answer("I think allow"))
        XCTAssertNil(VoiceGrammar.answer(""))
    }

    func testReadBackSpeaksTheCommand() {
        let r = PermissionRequest(command: "wrangler deploy --env prod", id: "r1", title: "Run wrangler deploy?", tool: "Bash")
        XCTAssertEqual(VoiceGrammar.speakCommand("wrangler deploy --env prod"), "wrangler deploy, dash dash env prod")
        XCTAssertEqual(VoiceGrammar.readBack(r, mayAllow: true), "Run wrangler deploy? wrangler deploy, dash dash env prod. Allow or deny?")
        XCTAssertEqual(VoiceGrammar.speakCommand("cd /srv/app && rm -rf build"), "cd slash srv slash app and then rm, dash rf build")
        XCTAssertEqual(VoiceGrammar.speakCommand("rm -rf ~/work/app/build"), "rm, dash rf home slash work slash app slash build")
        XCTAssertEqual(VoiceGrammar.speakCommand("make || echo failed; ls | wc"), "make or else echo failed then ls pipe wc")
        XCTAssertEqual(VoiceGrammar.speakCommand("rm -rf ~"), "rm, dash rf home")
        XCTAssertEqual(VoiceGrammar.speakCommand("rm *"), "rm star")
        XCTAssertEqual(VoiceGrammar.speakCommand("echo $HOME"), "echo dollar HOME")
        XCTAssertEqual(VoiceGrammar.speakCommand("tar --file=/tmp/a.tar"), "tar, dash dash file equals slash tmp slash a.tar")
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
        XCTAssertFalse(VoiceGrammar.voiceMayAllow(PermissionRequest(command: "echo $(whoami)", id: "r1"), now: now), "a command substitution")
        XCTAssertFalse(VoiceGrammar.voiceMayAllow(PermissionRequest(command: "echo `id`", id: "r1"), now: now))
    }
}
