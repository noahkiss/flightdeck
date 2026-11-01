import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Rich content (app.md 4.7): the list merge, the inline placement, the
/// offline placeholder, chunked reads and diff parsing.
@MainActor
final class ContentTests: XCTestCase {
    private func item(_ id: String, session: String = "s1", kind: ContentKind = .markdown, at: String = "2026-01-01T10:00:05Z",
                      shown: Bool? = true, change: ContentChange? = nil, message: String? = nil, size: Int? = 10) -> ContentItem {
        ContentItem(at: at, change: change, id: id, kind: kind, message: message, name: id + ".md", path: "/w/" + id,
                    session: session, shown: shown, size: size, type: "text/markdown")
    }

    private func msg(_ id: String, at: String, kind: ChatKind = .agent, parent: String? = nil) -> ChatMessage {
        ChatMessage(at: at, id: id, kind: kind, parent: parent, session: "s1", text: id)
    }

    func testMergeReplacesByIDAndKeepsChangeMark() {
        let modified = item("a", shown: nil, change: .modified, message: "m1")
        var list = ContentModel.merge([modified, item("b")], item("a", at: "2026-01-01T10:00:09Z"))
        XCTAssertEqual(list.map(\.id), ["b", "a"], "a show moves the item last")
        XCTAssertEqual(list[1].change, .modified, "the show keeps the change mark")
        XCTAssertEqual(list[1].message, "m1")
        XCTAssertEqual(list[1].shown, true)
        list = ContentModel.merge(list, item("c"))
        XCTAssertEqual(list.map(\.id), ["b", "a", "c"])
        // A list reply that raced an event keeps the event's item.
        let fresh = ContentModel.replace([item("b")], keeping: [item("d")])
        XCTAssertEqual(fresh.map(\.id), ["b", "d"])
    }

    func testPlacementUnderToolCallByTimeAndTrailing() {
        let messages = [
            msg("m1", at: "2026-01-01T10:00:00Z", kind: .user),
            msg("m2", at: "2026-01-01T10:00:02Z", kind: .tool),
            msg("m3", at: "2026-01-01T10:00:04Z", kind: .tool, parent: "m2"),
            msg("m4", at: "2026-01-01T10:00:06Z"),
            msg("m5", at: "2026-01-01T10:00:10Z"),
        ]
        let rows: Set<String> = ["m1", "m2", "m4", "m5"]
        let items = [
            item("img", kind: .image, shown: nil, message: "m2"),
            item("nested", kind: .image, shown: nil, message: "m3"),
            item("doc", at: "2026-01-01T10:00:07Z"),
            item("late", at: "2026-01-01T10:00:20Z"),
            item("changed", shown: nil, change: .modified, message: "m2"),
            item("lost", kind: .image, shown: nil, message: "gone"),
        ]
        let p = ContentPlacement.of(items: items, messages: messages, rowIDs: rows)
        XCTAssertEqual(p.byRow["m2"]?.map(\.id), ["img", "nested"], "a nested call's image goes under its subagent card")
        XCTAssertEqual(p.byRow["m4"]?.map(\.id), ["doc"], "a shown item goes under the last row at or before its time")
        XCTAssertEqual(p.trailing.map(\.id), ["late"])
        XCTAssertNil(p.byRow.values.flatMap { $0 }.first { $0.id == "changed" }, "a modified file shows in its card, not inline")
        XCTAssertFalse((p.byRow.values.flatMap { $0 } + p.trailing).contains { $0.id == "lost" })
    }

    func testDiffParsing() {
        let text = """
        diff --git a/src/a.go b/src/a.go
        index 1..2 100644
        --- a/src/a.go
        +++ b/src/a.go
        @@ -10,4 +10,5 @@ func A() {
         keep
        -old
        +new
        +more

        \\ No newline at end of file
        diff --git a/img.png b/img.png
        Binary files a/img.png and b/img.png differ
        --- /dev/null
        +++ b/new.md
        @@ -0,0 +1,2 @@
        +# New
        +line
        --- a/notes.txt
        +++ b/notes.txt
        @@ -1,1 +1,1 @@ edit 1
        -hello
        +hello world
        """
        let d = DiffParser.parse(text)
        XCTAssertEqual(d.files.count, 4)
        let a = d.files[0]
        XCTAssertEqual(a.oldPath, "src/a.go")
        XCTAssertEqual(a.hunks.count, 1)
        XCTAssertEqual(a.added, 2)
        XCTAssertEqual(a.removed, 1)
        let lines = a.hunks[0].lines
        XCTAssertEqual(lines.map(\.kind), [.context, .removed, .added, .added, .context, .note])
        XCTAssertEqual(lines[0].old, 10)
        XCTAssertEqual(lines[0].new, 10)
        XCTAssertEqual(lines[1].old, 11)
        XCTAssertEqual(lines[2].new, 11)
        XCTAssertEqual(lines[3].new, 12)
        XCTAssertTrue(d.files[1].binary)
        XCTAssertNil(d.files[2].oldPath, "/dev/null is no path")
        XCTAssertEqual(d.files[2].newPath, "new.md")
        XCTAssertEqual(d.files[2].added, 2)
        XCTAssertEqual(d.files[3].hunks[0].lines.map(\.text), ["hello", "hello world"])
        XCTAssertEqual(DiffParser.parse("").files.count, 0)
        // A hunk header with no line numbers (the hab's transcript diffs).
        let t = DiffParser.parse("--- a/x\n+++ b/x\n@@ edit 1: deleted @@\n")
        XCTAssertEqual(t.files.first?.hunks.first?.header, "@@ edit 1: deleted @@")
    }

    /// A fleet with one fake hab whose session s1 has a document and a modified file.
    private func setup() async -> (Fleet, FakeHab, ChatModel) {
        let hab = await sampleHab()
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .idle))
        await hab.setChat(session: "s1", [msg("m1", at: "2026-01-01T10:00:00Z", kind: .user)])
        let doc = item("doc", size: 12)
        let mod = item("mod", shown: nil, change: .modified, size: 3)
        await hab.setContent(session: "s1", items: [doc, mod], files: [doc.id: Data("# Hello\n\nabc\n".utf8)],
                             diffs: [mod.id: "--- a/mod\n+++ b/mod\n@@ -1 +1 @@\n-a\n+b\n"])
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await fleet.setActive(true)
        await eventually("online") { fleet.board.habs.first?.state.status == .online }
        let model = fleet.chat(for: SessionTarget(habID: "h", session: "s1", pane: "%1"))
        model.content.refreshDelay = .milliseconds(20)
        return (fleet, hab, model)
    }

    func testListEventsChunksAndOfflinePlaceholder() async {
        let (fleet, hab, model) = await setup()
        model.start()
        let content = model.content
        await eventually("list") { content.items.count == 2 }
        XCTAssertEqual(content.documents.map(\.id), ["doc"])
        XCTAssertEqual(content.modified.map(\.id), ["mod"])

        // A content.updated event merges in place.
        await hab.showContent(item("shot", kind: .image, at: "2026-01-01T10:00:09Z"))
        await eventually("event") { content.items.last?.id == "shot" }
        // Another session's item stays out.
        await hab.showContent(item("other", session: "s2"))
        try? await Task.sleep(for: .milliseconds(100))
        XCTAssertFalse(content.items.contains { $0.id == "other" })

        // Bytes come in chunks and join.
        await hab.setContentChunk(4)
        let doc = content.items.first { $0.id == "doc" }!
        let got = await content.bytes(for: doc)
        XCTAssertEqual(got, .data(Data("# Hello\n\nabc\n".utf8), changedWhileLoading: false))
        let reads = await hab.contentReads
        XCTAssertEqual(reads, 4, "13 bytes in runs of 4")

        // A diff parses.
        let mod = content.items.first { $0.id == "mod" }!
        if case .diff(let d, let source, _) = await content.diff(for: mod) {
            XCTAssertEqual(source, "git")
            XCTAssertEqual(d.files.first?.added, 1)
        } else {
            XCTFail("no diff")
        }
        // An id the hab does not list: gone, not an error.
        let missing = await content.bytes(for: item("nope"))
        XCTAssertEqual(missing, .gone)

        // Offline: the last list stays; loads answer with the placeholder, never an error.
        await fleet.setActive(false)
        await eventually("offline") { !model.context.online }
        XCTAssertEqual(content.items.count, 3)
        XCTAssertEqual(content.offlineText, "Not available while bench is offline")
        let off = await content.bytes(for: doc)
        XCTAssertEqual(off, .offline("Not available while bench is offline"))
        let offDiff = await content.diff(for: mod)
        XCTAssertEqual(offDiff, .offline("Not available while bench is offline"))
        model.stop()
    }

    func testFileChangedBetweenChunksReadsAgainThenMarks() async {
        let (fleet, hab, model) = await setup()
        model.start()
        let content = model.content
        await eventually("list") { content.items.count == 2 }
        await hab.setContentChunk(4)
        let doc = content.items.first { $0.id == "doc" }!
        // Every second read sees a new version: the first pass and the retry both break.
        let counter = Counter()
        await hab.setContentReadHook { [weak hab] _ in
            let n = await counter.next()
            if n % 2 == 0 {
                await hab?.setContentFile(id: "doc", data: Data("# Hello\n\nabc\n".utf8), modified: "v\(n)")
            }
        }
        let got = await content.bytes(for: doc)
        guard case .data(_, let changed) = got else { return XCTFail("\(got)") }
        XCTAssertTrue(changed, "a file that keeps changing shows with the mark")
        await fleet.setActive(false)
        model.stop()
    }

    func testDemoContentRendersOnTheTideSession() async {
        let png = DemoContent.chartPNG()
        XCTAssertGreaterThan(png.count, 100)
        XCTAssertNotNil(platformImage(png))
        XCTAssertEqual(DiffParser.parse(DemoContent.stationsDiff).files.first?.added, 1)
    }
}

actor Counter {
    private var n = 0
    func next() -> Int {
        n += 1
        return n
    }
}
