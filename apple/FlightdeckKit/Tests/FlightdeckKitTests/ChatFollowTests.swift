import Markdown
import SwiftUI
import XCTest
@testable import FlightdeckKit

/// The chat's scrolling and message menu rules (app.md 4.12): following the newest row, the
/// jump-to-latest count, the unread divider, the menu's actions and the Return key.
final class ChatFollowTests: XCTestCase {
    private let tail = (16...25).map { "m\($0)" }

    /// The list keeps its place on a prepend, and follows new rows only from the bottom.
    func testRowsFollowOnlyFromTheBottom() {
        var f = ChatFollow()
        XCTAssertEqual(f.rowsChanged(from: [], to: tail), .toBottom, "first load")
        XCTAssertEqual(f.rowsChanged(from: tail, to: (6...25).map { "m\($0)" }), .keep("m16"), "older page at the bottom")
        XCTAssertEqual(f.rowsChanged(from: tail, to: tail), .none, "a change in place")
        XCTAssertEqual(f.rowsChanged(from: tail, to: tail + ["m26"]), .toBottom, "following")

        XCTAssertEqual(f.scrolled(distance: 400, motion: .user), .none)
        XCTAssertFalse(f.following, "the owner scrolled up")
        XCTAssertEqual(f.rowsChanged(from: tail, to: (6...25).map { "m\($0)" }), .keep("m16"), "older page while reading")
        XCTAssertEqual(f.rowsChanged(from: tail, to: tail + ["m26", "m27"]), .none, "reading history: nothing moves")
        XCTAssertEqual(f.unread, 2)
        XCTAssertEqual(f.rowsChanged(from: tail + ["m26", "m27"], to: tail + ["m26", "m27", "m28"]), .none)
        XCTAssertEqual(f.unread, 3)
        XCTAssertTrue(f.showsJump)

        XCTAssertEqual(f.rowsChanged(from: tail, to: ["x1", "x9"]), .toBottom, "a new conversation")
        XCTAssertTrue(f.following)
        XCTAssertEqual(f.unread, 0)
    }

    /// Following is sticky: growth under a streaming reply, the keyboard or a taller composer
    /// never clears it; only the owner's own scroll past the tolerance does. A following thread
    /// goes back to the bottom once no finger moves it, never during the drag.
    func testFollowingIsSticky() {
        var f = ChatFollow()
        XCTAssertEqual(f.scrolled(distance: 0, motion: .idle), .none)
        XCTAssertEqual(f.scrolled(distance: 300, motion: .idle), .toBottom, "a reply grew by a tall block")
        XCTAssertTrue(f.following)
        XCTAssertEqual(f.scrolled(distance: 2, motion: .user), .none, "a nudge stays at the bottom")
        XCTAssertTrue(f.following)
        XCTAssertEqual(f.scrolled(distance: 120, motion: .animating), .none, "the app's own scroll")
        XCTAssertTrue(f.following)
        // The drag that takes the keyboard away changes the insets on every frame.
        XCTAssertEqual(f.scrolled(distance: -30, motion: .user), .none)
        XCTAssertTrue(f.following)

        // A request card or the keyboard changes the bottom inset: back to the end, never mid-drag.
        XCTAssertEqual(f.insetChanged(motion: .idle), .toBottom)
        XCTAssertEqual(f.insetChanged(motion: .user), .none)

        XCTAssertEqual(f.scrolled(distance: ChatFollow.tolerance + 1, motion: .user), .none)
        XCTAssertFalse(f.following)
        XCTAssertEqual(f.insetChanged(motion: .idle), .none, "reading: the bars do not pull the owner down")
        XCTAssertEqual(f.scrolled(distance: 600, motion: .idle), .none, "reading: content growth does not pull the owner down")
        XCTAssertEqual(f.scrolled(distance: 10, motion: .user), .none)
        XCTAssertTrue(f.following, "back at the bottom by hand")
    }

    /// A scroll that is not a finger (status-bar tap, Page Up, Home, VoiceOver, the Mac's scroller
    /// or wheel) moves away while content and bars keep their size: the owner's, in any phase.
    func testScrollsWithoutAFingerStopFollowing() {
        typealias S = ChatFollow.Sample
        var f = ChatFollow()
        let rest = S(distance: 0, contentHeight: 2000, bottomInset: 120)
        // A status-bar tap animates to the top, then the phase goes idle: no snap back.
        XCTAssertEqual(f.scrolled(from: rest, to: S(distance: 900, contentHeight: 2000, bottomInset: 120), motion: .animating), .none)
        XCTAssertFalse(f.following)
        XCTAssertEqual(f.scrolled(distance: 1500, motion: .idle), .none)

        // Page Up with no phase at all.
        var g = ChatFollow()
        XCTAssertEqual(g.scrolled(from: rest, to: S(distance: 600, contentHeight: 2000, bottomInset: 120), motion: .idle), .none)
        XCTAssertFalse(g.following)

        // Content that grows, or a bar that grows, is not the owner: back to the end.
        var h = ChatFollow()
        XCTAssertEqual(h.scrolled(from: rest, to: S(distance: 300, contentHeight: 2300, bottomInset: 120), motion: .idle), .toBottom)
        XCTAssertEqual(h.scrolled(from: rest, to: S(distance: 300, contentHeight: 2000, bottomInset: 420), motion: .idle), .toBottom)
        XCTAssertTrue(h.following)

        // A shorter view (window resize, rotation, Split View) is not the owner either.
        var k = ChatFollow()
        let tall = S(distance: 0, contentHeight: 2000, bottomInset: 120, containerHeight: 800)
        XCTAssertEqual(k.scrolled(from: tall, to: S(distance: 300, contentHeight: 2000, bottomInset: 120, containerHeight: 500), motion: .idle), .toBottom)
        XCTAssertTrue(k.following)
    }

    /// Rows left a bottom inset too high, with the scroll geometry reading "at the bottom",
    /// go back to the end while the thread follows, and only then.
    func testGapUnderTheRows() {
        // Read from the simulator: a phone thread, 1260 pt of rows, a 387 pt view between the
        // bars. At rest at its end, and stuck 371 pt (the request card and composer) too high.
        XCTAssertEqual(ChatFollow.gap(contentHeight: 1260, visible: CGRect(x: 0, y: 873, width: 402, height: 387)), 0)
        XCTAssertEqual(ChatFollow.gap(contentHeight: 1260, visible: CGRect(x: 0, y: 1244, width: 402, height: 387)), 371)
        XCTAssertEqual(ChatFollow.gap(contentHeight: 1260, visible: CGRect(x: 0, y: 200, width: 402, height: 387)), 0, "scrolled up")
        XCTAssertEqual(ChatFollow.gap(contentHeight: 300, visible: CGRect(x: 0, y: 0, width: 402, height: 387)), 0, "rows that fit the view")

        var f = ChatFollow()
        XCTAssertEqual(f.placed(gap: 371, motion: .idle), .toBottom)
        XCTAssertEqual(f.placed(gap: ChatFollow.gapTolerance, motion: .idle), .none)
        XCTAssertEqual(f.placed(gap: 371, motion: .user), .none, "the finger pulls past the end")
        XCTAssertEqual(f.placed(gap: 371, motion: .animating), .none, "the app's own scroll")

        _ = f.scrolled(distance: 500, motion: .user)
        XCTAssertEqual(f.placed(gap: 371, motion: .idle), .none, "reading: nothing pulls the owner down")
    }

    /// Only messages count as new: tool cards and event lines do not.
    func testUnreadCountsMessagesOnly() {
        var f = ChatFollow()
        _ = f.rowsChanged(from: [], to: tail)
        _ = f.scrolled(distance: 500, motion: .user)
        _ = f.rowsChanged(from: tail, to: tail + ["tool1", "reply", "event"]) { $0 == "reply" }
        XCTAssertEqual(f.unread, 1)
    }

    /// Raw HTML in agent text or a Markdown file never reaches the renderer (app.md 4.6): an HTML
    /// block becomes a code block and inline HTML a code span, with the markup kept as text.
    func testHTMLBecomesText() {
        let md = """
        <p align="center"><img src="https://example.com/x.png"></p>

        Before <img src="https://example.com/?d=1"> after, and a <br> break.

        <script>location='https://example.com'</script>

        Plain `code` and **bold**.
        """
        let doc = Markdown.Document(parsing: md)
        XCTAssertTrue(ChatMarkdown.containsHTML(doc))
        let inert = ChatMarkdown.withoutHTML(doc)
        XCTAssertFalse(ChatMarkdown.containsHTML(inert))
        let codes = ChatMarkdown.codeBlocks(inert.format())
        XCTAssertTrue(codes.contains { $0.contains("<p align=\"center\">") }, "the block shows as code: \(codes)")
        XCTAssertTrue(codes.contains { $0.contains("<script>") })
        XCTAssertTrue(inert.format().contains("`<img src=\"https://example.com/?d=1\">`"), inert.format())
        let plain = Markdown.Document(parsing: "No HTML, only `a < b`.")
        XCTAssertFalse(ChatMarkdown.containsHTML(plain))
    }

    /// Links in agent text open only web and mail addresses (app.md 4.6).
    func testLinkSchemes() {
        for ok in ["https://example.com", "http://example.com", "mailto:someone@example.com", "HTTPS://example.com"] {
            XCTAssertTrue(ChatMarkdown.opensLink(URL(string: ok)!), ok)
        }
        for no in ["flightdeck://pair?code=1", "file:///Applications/App.app", "shortcuts://run-shortcut?name=x", "javascript:alert(1)"] {
            XCTAssertFalse(ChatMarkdown.opensLink(URL(string: no)!), no)
        }
    }

    /// A CJK input method keeps plain Return for itself; Cmd-Return still sends.
    func testReturnSendsLanguages() {
        XCTAssertTrue(ComposerKeys.returnSends(languages: ["en-US", "de-DE"]))
        XCTAssertFalse(ComposerKeys.returnSends(languages: ["en-US", "ja-JP"]))
        XCTAssertFalse(ComposerKeys.returnSends(languages: ["zh-Hans"]))
        XCTAssertFalse(ComposerKeys.returnSends(languages: ["ko-KR"]))
    }

    func testJumpAndSend() {
        var f = ChatFollow()
        _ = f.rowsChanged(from: [], to: tail)
        _ = f.scrolled(distance: 500, motion: .user)
        _ = f.rowsChanged(from: tail, to: tail + ["m26"])
        XCTAssertEqual(f.unread, 1)
        XCTAssertEqual(f.jump(), .toBottom)
        XCTAssertTrue(f.following)
        XCTAssertEqual(f.unread, 0)
        XCTAssertFalse(f.showsJump)
        XCTAssertEqual(ChatFollow.jumpLabel(0), "Jump to latest")
        XCTAssertEqual(ChatFollow.jumpLabel(1), "1 new message")
        XCTAssertEqual(ChatFollow.jumpLabel(4), "4 new messages")
    }

    /// The distance counts the bars over the thread, which sit in its bottom inset.
    func testDistanceCountsTheInsets() {
        // Read from the simulator: a long thread at rest at its bottom, under a 371 pt bar
        // (a request card and the composer), and the same thread 200 pt up.
        XCTAssertEqual(ChatFollow.distance(contentHeight: 1236, visibleMaxY: 1607, bottomInset: 371), 0)
        XCTAssertEqual(ChatFollow.distance(contentHeight: 1236, visibleMaxY: 1407, bottomInset: 371), 200)
        // A short thread rests above the line: at the bottom.
        XCTAssertEqual(ChatFollow.distance(contentHeight: 410, visibleMaxY: 621, bottomInset: 136), 0)
    }

    @MainActor
    func testUnreadDivider() {
        let rows = ["a", "b", "c", "d"]
        XCTAssertNil(ChatReadMarks.divider(rows: rows, seen: nil), "never seen")
        XCTAssertEqual(ChatReadMarks.divider(rows: rows, seen: "b"), "c")
        XCTAssertNil(ChatReadMarks.divider(rows: rows, seen: "d"), "nothing new")
        XCTAssertNil(ChatReadMarks.divider(rows: rows, seen: "z"), "another tail")
        let marks = ChatReadMarks()
        marks.saw("c", in: "h|s1")
        marks.saw(nil, in: "h|s1")
        XCTAssertEqual(marks.seen(in: "h|s1"), "c")
        XCTAssertNil(marks.seen(in: "h|s2"))
    }

    func testMessageMenu() {
        let reply = ChatItem.agent(id: "a", text: "Run:\n\n```sh\nmake test\n```\n\nThen `ok`.", summary: nil, sender: nil)
        let c = try! XCTUnwrap(MessageMenu.content(reply))
        XCTAssertEqual(MessageMenu.actions(c), [.copy, .selectText, .share, .readAloud, .copyLastCode])
        XCTAssertEqual(MessageMenu.copied(.copy, c), c.text, "Copy takes the Markdown as written")
        XCTAssertEqual(MessageMenu.copied(.copyLastCode, c), "make test")
        XCTAssertFalse(c.speech.contains("```"), "Read Aloud drops the marks")

        let plain = try! XCTUnwrap(MessageMenu.content(.agent(id: "b", text: "Done.", summary: "All good.", sender: nil)))
        XCTAssertEqual(MessageMenu.actions(plain), [.copy, .selectText, .share, .readAloud])
        XCTAssertEqual(plain.text, "Done.", "the folded summary stays out")

        XCTAssertNil(MessageMenu.content(.agent(id: "c", text: "  ", summary: nil, sender: nil)))
        // Indented code has no fence: the menu does not offer it (a scan, not a parse, per redraw).
        let indented = try! XCTUnwrap(MessageMenu.content(.agent(id: "d", text: "Run:\n\n    make", summary: nil, sender: nil)))
        XCTAssertFalse(MessageMenu.actions(indented).contains(.copyLastCode))
        XCTAssertNil(MessageMenu.content(.event(id: "e", event: .compaction, text: "Compacted")))
        let user = try! XCTUnwrap(MessageMenu.content(.user(id: "u", text: "hi", sender: nil, attachments: [])))
        XCTAssertEqual(MessageMenu.actions(user), [.copy, .selectText, .share, .readAloud])

        let capcom = try! XCTUnwrap(MessageMenu.content(CapcomRow.capcom(id: "k", text: "**Yes.**", wroteFirst: false)))
        XCTAssertEqual(MessageMenu.copied(.copy, capcom), "**Yes.**")
        XCTAssertNil(MessageMenu.content(CapcomRow.note(id: "n", text: "note")))
    }

    func testToolCardMenu() {
        let card = ToolCard(name: "Bash", target: nil, status: .done, sentence: ToolSentence(verb: "Ran"), kind: .commands, oneLine: false,
                            detail: .command("go test ./..."), fields: [], output: "ok", outputTruncated: false, children: [])
        let c = try! XCTUnwrap(MessageMenu.content(.tool(id: "t", card: card)))
        XCTAssertEqual(MessageMenu.actions(c), [.copyCommand, .copyOutput])
        XCTAssertEqual(MessageMenu.copied(.copyCommand, c), "go test ./...")
        XCTAssertEqual(MessageMenu.copied(.copyOutput, c), "ok")
        var quiet = card
        quiet.detail = .none
        quiet.output = nil
        XCTAssertNil(MessageMenu.content(.tool(id: "t", card: quiet)), "nothing to copy: no menu")
    }

    func testReturnKey() {
        XCTAssertEqual(ComposerKeys.returnKey([]), .send)
        XCTAssertEqual(ComposerKeys.returnKey(.command), .send)
        XCTAssertEqual(ComposerKeys.returnKey(.shift), .newline)
        XCTAssertEqual(ComposerKeys.returnKey(.option), .newline)

        let (end, _) = ComposerKeys.insertingNewline("abc", at: nil)
        XCTAssertEqual(end, "abc\n")
        let text = "hello world"
        let mid = text.index(text.startIndex, offsetBy: 5)
        let (split, caret) = ComposerKeys.insertingNewline(text, at: TextSelection(insertionPoint: mid))
        XCTAssertEqual(split, "hello\n world")
        guard case .selection(let r) = caret.indices else { return XCTFail("no caret") }
        XCTAssertEqual(split.distance(from: split.startIndex, to: r.lowerBound), 6)
        let sel = text.index(text.startIndex, offsetBy: 5)..<text.endIndex
        XCTAssertEqual(ComposerKeys.insertingNewline(text, at: TextSelection(range: sel)).0, "hello\n", "a selection is replaced")
    }
}
