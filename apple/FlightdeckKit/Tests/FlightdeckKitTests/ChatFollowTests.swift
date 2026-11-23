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

        XCTAssertEqual(f.scrolled(distance: ChatFollow.tolerance + 1, motion: .user), .none)
        XCTAssertFalse(f.following)
        XCTAssertEqual(f.scrolled(distance: 600, motion: .idle), .none, "reading: content growth does not pull the owner down")
        XCTAssertEqual(f.scrolled(distance: 10, motion: .user), .none)
        XCTAssertTrue(f.following, "back at the bottom by hand")
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
        // 1000 pt of rows in a 600 pt view, under an 80 pt composer: the bottom offset is 480.
        XCTAssertEqual(ChatFollow.distance(contentHeight: 1000, offsetY: 480, containerHeight: 600, topInset: 0, bottomInset: 80), 0)
        XCTAssertEqual(ChatFollow.distance(contentHeight: 1000, offsetY: 400, containerHeight: 600, topInset: 0, bottomInset: 80), 80)
        // A short thread is at the bottom wherever it rests: anchored low or high.
        XCTAssertEqual(ChatFollow.distance(contentHeight: 100, offsetY: -50, containerHeight: 600, topInset: 50, bottomInset: 80), 0)
        XCTAssertEqual(ChatFollow.distance(contentHeight: 100, offsetY: -420, containerHeight: 600, topInset: 50, bottomInset: 80), 0)
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
