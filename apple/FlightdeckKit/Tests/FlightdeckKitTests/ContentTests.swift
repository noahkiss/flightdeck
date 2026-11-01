import Foundation
import HabAPI
import ImageIO
import PDFKit
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
        let counter = ReadCounter()
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
        XCTAssertNotNil(ImageDecode.thumbnail(png, maxPixel: 256))
        XCTAssertEqual(DiffParser.parse(DemoContent.stationsDiff).files.first?.added, 1)
    }
}

actor ReadCounter {
    private var n = 0
    func next() -> Int {
        n += 1
        return n
    }
}

// MARK: - Fix round: limits, caches, decoding

@MainActor
final class ContentLimitTests: XCTestCase {
    private func item(_ id: String, size: Int, modified: String = "v1") -> ContentItem {
        ContentItem(at: "2026-01-01T10:00:05Z", id: id, kind: .text, modified: modified, name: id + ".txt", path: "/w/" + id,
                    session: "s1", shown: true, size: size, type: "text/plain")
    }

    /// A fleet with one fake hab whose session s1 has `n` small text files.
    private func setup(files n: Int) async -> (Fleet, FakeHab, ContentModel, [ContentItem]) {
        let hab = await sampleHab()
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .idle))
        await hab.setChat(session: "s1", [])
        let items = (0..<n).map { item("f\($0)", size: 5) }
        await hab.setContent(session: "s1", items: items, files: Dictionary(uniqueKeysWithValues: items.map { ($0.id, Data("hello".utf8)) }))
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await fleet.setActive(true)
        await eventually("online") { fleet.board.habs.first?.state.status == .online }
        let model = fleet.chat(for: SessionTarget(habID: "h", session: "s1", pane: "%1"))
        await eventually("context") { model.content.context.online }
        model.content.busyBackoff = .milliseconds(10)
        return (fleet, hab, model.content, items)
    }

    /// B1: eight cards loading at once stay under the hab's limit of four content calls per
    /// connection, and every one gets its bytes.
    func testManyCardsStayUnderTheHabLimit() async {
        let (fleet, hab, content, items) = await setup(files: 8)
        await hab.setContentDelay(.milliseconds(60))
        let results = await withTaskGroup(of: ContentBytes.self) { g in
            for it in items { g.addTask { await content.bytes(for: it) } }
            var out: [ContentBytes] = []
            for await r in g { out.append(r) }
            return out
        }
        XCTAssertEqual(results.filter { $0 == .data(Data("hello".utf8), changedWhileLoading: false) }.count, 8, "\(results)")
        let peak = await hab.contentPeak
        XCTAssertLessThanOrEqual(peak, HabConnection.contentSlots)
        await fleet.setActive(false)
    }

    /// B1: views asking for the same file at once share one load.
    func testSameFileLoadsOnce() async {
        let (fleet, hab, content, items) = await setup(files: 1)
        await hab.setContentDelay(.milliseconds(60))
        async let a = content.bytes(for: items[0])
        async let b = content.bytes(for: items[0])
        let (ra, rb) = await (a, b)
        XCTAssertEqual(ra, rb)
        let reads = await hab.contentReads
        XCTAssertEqual(reads, 1)
        await fleet.setActive(false)
    }

    /// B1: a busy answer is tried again; one that stays busy shows a plain line, never the hab's hint.
    func testBusyRetriedAndHintHidden() async {
        let (fleet, hab, content, items) = await setup(files: 2)
        await hab.failNextContentReads(2)
        let ok = await content.bytes(for: items[0])
        XCTAssertEqual(ok, .data(Data("hello".utf8), changedWhileLoading: false))
        await hab.failNextContentReads(100)
        let failed = await content.bytes(for: items[1])
        XCTAssertEqual(failed, .failed(ContentModel.loadFailed))
        await fleet.setActive(false)
    }

    /// R4: an `unavailable` that is not the busy limit (a hab with no content service) fails at
    /// once, with no retry waits.
    func testUnavailableNotBusyFailsFast() async {
        let (fleet, hab, content, items) = await setup(files: 1)
        content.busyBackoff = .seconds(2)
        await hab.setCapability("content", false)
        let start = ContinuousClock.now
        let r = await content.bytes(for: items[0])
        XCTAssertEqual(r, .failed(ContentModel.loadFailed))
        let d = await content.diff(for: items[0])
        XCTAssertEqual(d, .failed(ContentModel.loadFailed))
        XCTAssertLessThan(ContinuousClock.now - start, .seconds(1), "no retry wait for a hab with no content service")
        XCTAssertTrue(ContentModel.isBusy(RPCError(code: 1, message: "", data: ErrorData(hint: "4 content calls already run on this connection", kind: .unavailable, seq: 0))))
        XCTAssertFalse(ContentModel.isBusy(RPCError(code: 1, message: "", data: ErrorData(hint: "/w/a: input/output error", kind: .unavailable, seq: 0))))
        await fleet.setActive(false)
    }

    /// S2: a file rewritten at the same size, with a new modified time, loads again.
    func testSameSizeRewriteLoadsAgain() async {
        let (fleet, hab, content, items) = await setup(files: 1)
        let first = await content.bytes(for: items[0])
        XCTAssertEqual(first, .data(Data("hello".utf8), changedWhileLoading: false))
        let newer = item("f0", size: 5, modified: "v2")
        await hab.setContent(session: "s1", items: [newer])
        await hab.setContentFile(id: "f0", data: Data("world".utf8), modified: "v2")
        await content.refresh()
        XCTAssertEqual(content.items.first?.modified, "v2")
        let second = await content.bytes(for: newer)
        XCTAssertEqual(second, .data(Data("world".utf8), changedWhileLoading: false))
        await fleet.setActive(false)
    }

    /// N7: a list reply that a later call's reply already replaced is dropped.
    func testOlderListReplyDropped() async {
        let (fleet, hab, content, items) = await setup(files: 1)
        await hab.setContentListHook { n in
            if n == 1 { try? await Task.sleep(for: .milliseconds(300)) }
        }
        let slow = Task { await content.refresh() }
        try? await Task.sleep(for: .milliseconds(50))
        await hab.setContent(session: "s1", items: items + [item("f9", size: 5)])
        await content.refresh()
        XCTAssertEqual(content.items.count, 2)
        await slow.value
        XCTAssertEqual(content.items.count, 2, "the older reply must not replace the newer list")
        await fleet.setActive(false)
    }

    /// S2: the byte cache holds one version per item and stays under its byte limit,
    /// dropping the least recently used first.
    func testByteCacheBoundAndVersions() {
        let c = ByteCache(limit: 10)
        let k1 = ByteCache.Key(id: "a", size: 4, modified: "v1")
        c.put(id: "a", key: k1, data: Data(count: 4))
        XCTAssertNotNil(c.get(id: "a", key: k1))
        XCTAssertNil(c.get(id: "a", key: ByteCache.Key(id: "a", size: 4, modified: "v2")), "another version is a miss")
        c.put(id: "b", key: ByteCache.Key(id: "b", size: 4, modified: nil), data: Data(count: 4))
        _ = c.get(id: "a", key: k1) // a is now the most recent
        c.put(id: "c", key: ByteCache.Key(id: "c", size: 4, modified: nil), data: Data(count: 4))
        XCTAssertLessThanOrEqual(c.total, 10)
        XCTAssertNil(c.get(id: "b", key: ByteCache.Key(id: "b", size: 4, modified: nil)), "the least recently used went first")
        XCTAssertNotNil(c.get(id: "a", key: k1))
        c.put(id: "big", key: ByteCache.Key(id: "big", size: 11, modified: nil), data: Data(count: 11))
        XCTAssertEqual(c.count, 2, "bytes over the whole limit are not held")
        c.keep(id: "a", key: ByteCache.Key(id: "a", size: 4, modified: "v2"))
        XCTAssertNil(c.get(id: "a", key: k1), "a list that names a new version drops the old bytes")
    }

    /// R3: decoded images are bounded by bitmap bytes, then by count; the least recently used
    /// goes first, and the newest stays even when it alone passes the byte bound.
    func testImageCacheBoundByBytes() throws {
        func image(_ side: Int) throws -> CGImage {
            let ctx = try XCTUnwrap(CGContext(data: nil, width: side, height: side, bitsPerComponent: 8, bytesPerRow: side * 4,
                                              space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue))
            return try XCTUnwrap(ctx.makeImage())
        }
        let small = try image(10) // 400 bytes
        let c = ImageCache(byteLimit: 1000, countLimit: 3)
        c.put("a", small)
        c.put("b", small)
        XCTAssertEqual(c.total, 800)
        _ = c.get("a") // a is now the most recent
        c.put("c", small)
        XCTAssertLessThanOrEqual(c.total, 1000)
        XCTAssertNil(c.get("b"), "the least recently used went first")
        XCTAssertNotNil(c.get("a"))
        let big = try image(20) // 1600 bytes, past the whole bound
        c.put("z", big)
        XCTAssertEqual(c.count, 1, "the newest stays alone")
        XCTAssertNotNil(c.get("z"))
        let counted = ImageCache(byteLimit: 1 << 30, countLimit: 2)
        for k in ["a", "b", "c"] { counted.put(k, small) }
        XCTAssertEqual(counted.count, 2, "count is the second bound")
        XCTAssertEqual(ImageCache.budget(physicalMemory: 4 << 30), 128 << 20)
        XCTAssertEqual(ImageCache.budget(physicalMemory: 1 << 30), 64 << 20)
        XCTAssertEqual(ImageCache.budget(physicalMemory: 64 << 30), 256 << 20)
        c.removeAll()
        XCTAssertEqual(c.count, 0)
        XCTAssertEqual(c.total, 0)
    }

    /// S1: every chat shares one image cache, and the same item id on two habs is two images.
    func testImageCacheSharedAcrossChats() async {
        let png = DemoContent.chartPNG()
        let a = ContentModel(target: SessionTarget(habID: "h1", session: "s1", pane: nil), items: [], context: ChatContext())
        let b = ContentModel(target: SessionTarget(habID: "h2", session: "s1", pane: nil), items: [], context: ChatContext())
        XCTAssertTrue(a.decoded === b.decoded, "one cache for the device")
        let it = ContentItem(at: "2026-01-01T10:00:05Z", id: "img", kind: .image, modified: "v1", name: "c.png", path: "/w/c.png",
                             session: "s1", shown: true, size: png.count, type: "image/png")
        let ka = a.imageKey(for: it, data: png, maxPixel: 64)
        let kb = b.imageKey(for: it, data: png, maxPixel: 64)
        XCTAssertNotEqual(ka, kb, "the same id on two habs is kept apart")
        // Each entry is checked right after its own put, not by a count on the process-wide
        // cache, which a memory-pressure purge may empty at any time.
        let ia = await a.image(for: it, data: png, maxPixel: 64)
        XCTAssertNotNil(ia)
        XCTAssertNotNil(a.decoded.get(ka))
        let ib = await b.image(for: it, data: png, maxPixel: 64)
        XCTAssertNotNil(ib)
        XCTAssertNotNil(b.decoded.get(kb))
    }

    /// R6: a diff file is parsed, and a text file split into runs, once per version: redraws
    /// reuse them, and a new version parses again.
    func testDiffAndRunsParsedOncePerVersion() {
        let m = ContentModel(target: SessionTarget(habID: "h", session: "s1", pane: "%1"), items: [], context: ChatContext())
        let diff = Data("--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n".utf8)
        let v1 = item("d", size: diff.count)
        for _ in 0..<5 { XCTAssertEqual(m.parsedDiff(for: v1, data: diff).files.first?.added, 1) }
        XCTAssertEqual(m.diffParses, 1)
        _ = m.parsedDiff(for: item("d", size: diff.count, modified: "v2"), data: diff)
        XCTAssertEqual(m.diffParses, 2, "a new version parses again")
        let text = Data("a\nb\nc".utf8)
        for _ in 0..<5 { XCTAssertEqual(m.textChunks(for: item("t", size: text.count), data: text), ["a\nb\nc"]) }
        XCTAssertEqual(m.chunkSplits, 1)
    }

    /// M1: while a same-size rewrite loads, the old bytes show under the version they were
    /// loaded for, so the new version's parse comes from the new bytes. Bytes that changed while
    /// loading get a key of their own.
    func testCachesKeyOnTheLoadedVersion() {
        let m = ContentModel(target: SessionTarget(habID: "h", session: "s1", pane: "%1"), items: [], context: ChatContext())
        let a = Data("--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n".utf8)
        let b = Data("--- a/x\n+++ b/x\n@@ -1 +1 @@\n-c\n+d\n".utf8)
        XCTAssertEqual(a.count, b.count)
        let v1 = item("d", size: a.count), v2 = item("d", size: a.count, modified: "v2")
        // The loader asked for v1 and got a; the list then moves to v2 before b arrives.
        let shownWhileLoading = ContentModel.version(v1, of: .data(a, changedWhileLoading: false))
        XCTAssertEqual(shownWhileLoading, v1)
        XCTAssertEqual(m.parsedDiff(for: shownWhileLoading, data: a).files.first?.hunks.first?.lines.last?.text, "b")
        XCTAssertEqual(m.markdown(for: shownWhileLoading, data: a).isEmpty, false)
        let loaded = ContentModel.version(v2, of: .data(b, changedWhileLoading: false))
        XCTAssertEqual(m.parsedDiff(for: loaded, data: b).files.first?.hunks.first?.lines.last?.text, "d", "v2 shows v2's bytes")
        // A Markdown parse keys on the byte count too.
        let mdOld = Data("# old\n".utf8), mdNew = Data("# newer\n".utf8)
        _ = m.markdown(for: v1, data: mdOld)
        XCTAssertNotEqual(m.markdown(for: v1, data: mdNew), m.markdown(for: v1, data: mdOld))
        let changed = ContentModel.version(v2, of: .data(b, changedWhileLoading: true))
        XCTAssertNotEqual(ByteCache.Key(changed), ByteCache.Key(v2), "bytes that changed while loading never answer for v2")
    }

    /// R5: Markdown past the preview limit renders cut, and says so; the whole file is there as
    /// text runs for "Show all as text".
    func testLargeMarkdownMarkedCut() {
        let m = ContentModel(target: SessionTarget(habID: "h", session: "s1", pane: "%1"), items: [], context: ChatContext())
        let para = "Line of text " + String(repeating: "x", count: 86) + "\n\n" // 101 bytes
        let big = Data(String(repeating: para, count: 3000).utf8) // about 300 KB
        let it = ContentItem(at: "2026-01-01T10:00:05Z", id: "md", kind: .markdown, modified: "v1", name: "big.md", path: "/w/big.md",
                             session: "s1", shown: true, size: big.count, type: "text/markdown")
        XCTAssertTrue(ContentModel.markdownCut(big))
        XCTAssertFalse(ContentModel.markdownCut(Data(para.utf8)))
        XCTAssertFalse(ContentModel.markdownCut(Data(count: ContentText.previewLimit)), "exactly the limit renders whole")
        let blocks = m.markdown(for: it, data: big)
        XCTAssertLessThan(blocks.count, 3000, "only the first part is parsed")
        XCTAssertTrue(DocumentBody.size(ContentText.previewLimit).hasPrefix("256"), "the cut line reads 256, not 262: \(DocumentBody.size(ContentText.previewLimit))")
        // One long line with no newline: the cut never splits a character.
        let long = Data(("ab" + String(repeating: "é", count: 10)).utf8) // é is 2 bytes
        for limit in 3...12 {
            let p = ContentText.preview(long, limit: limit)
            XCTAssertTrue(p.truncated)
            XCTAssertFalse(p.text.contains("\u{FFFD}"), "limit \(limit) split a character")
            XCTAssertLessThanOrEqual(p.text.utf8.count, limit)
        }
        XCTAssertEqual(m.textChunks(for: it, data: big).joined(separator: "\n").utf8.count, big.count, "the runs hold the whole file")
    }

    /// S3: a large image decodes to the asked bound, not to its full size.
    func testImageDecodesToBound() throws {
        let w = 3000, h = 1200
        let ctx = try XCTUnwrap(CGContext(data: nil, width: w, height: h, bitsPerComponent: 8, bytesPerRow: 0,
                                          space: CGColorSpaceCreateDeviceRGB(), bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue))
        ctx.setFillColor(CGColor(red: 1, green: 0, blue: 0, alpha: 1))
        ctx.fill(CGRect(x: 0, y: 0, width: w, height: h))
        let out = NSMutableData()
        let dest = try XCTUnwrap(CGImageDestinationCreateWithData(out, "public.png" as CFString, 1, nil))
        CGImageDestinationAddImage(dest, try XCTUnwrap(ctx.makeImage()), nil)
        CGImageDestinationFinalize(dest)
        let img = try XCTUnwrap(ImageDecode.thumbnail(out as Data, maxPixel: 512))
        XCTAssertEqual(max(img.width, img.height), 512)
        XCTAssertEqual(img.height, 205, accuracy: 1)
    }

    /// S4: a huge text renders a bounded preview cut at a line end, and says more follows.
    func testTextPreviewBounded() {
        let line = String(repeating: "x", count: 99) + "\n"
        let data = Data(String(repeating: line, count: 10_000).utf8) // 1 MB
        let p = ContentText.preview(data, limit: 1000)
        XCTAssertTrue(p.truncated)
        XCTAssertLessThanOrEqual(p.text.utf8.count, 1000)
        XCTAssertTrue(p.text.hasSuffix("x"), "cut at a line end")
        XCTAssertFalse(ContentText.preview(Data("short".utf8)).truncated)
        XCTAssertEqual(ContentText.chunks(Data("a\nb\nc".utf8), lines: 2), ["a\nb", "c"])
    }

    /// S4: a parent redraw with the same PDF bytes keeps the document.
    func testPDFKeepsDocumentOnRedraw() {
        let src = PDFSource()
        let d = Data("%PDF-1.4".utf8)
        XCTAssertTrue(src.needsLoad(d))
        let v = PDFView()
        src.show(d, in: v)
        src.show(d, in: v)
        XCTAssertEqual(src.loads, 1)
        src.show(Data("%PDF-1.5".utf8), in: v)
        XCTAssertEqual(src.loads, 2)
    }
}
