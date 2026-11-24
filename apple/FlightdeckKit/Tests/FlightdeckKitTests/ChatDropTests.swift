import CoreGraphics
import Foundation
import ImageIO
import UniformTypeIdentifiers
import XCTest
@testable import FlightdeckKit

/// Dropping files on a chat (app.md 4.4): what a drop adds to the composer, the refusals, and
/// when the composer takes files at all.
final class ChatDropTests: XCTestCase {
    private var dir: URL!

    override func setUp() {
        dir = FileManager.default.temporaryDirectory.appendingPathComponent("chatdrop-\(UUID().uuidString)")
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
    }

    override func tearDown() {
        try? FileManager.default.removeItem(at: dir)
    }

    private func write(_ name: String, _ data: Data) -> URL {
        let url = dir.appendingPathComponent(name)
        try? data.write(to: url)
        return url
    }

    private func png(width: Int, height: Int) -> Data {
        let ctx = CGContext(data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
                            space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
        ctx.setFillColor(CGColor(red: 0, green: 0, blue: 1, alpha: 1))
        ctx.fill(CGRect(x: 0, y: 0, width: width, height: height))
        let out = NSMutableData()
        let dst = CGImageDestinationCreateWithData(out, UTType.png.identifier as CFString, 1, nil)!
        CGImageDestinationAddImage(dst, ctx.makeImage()!, nil)
        CGImageDestinationFinalize(dst)
        return out as Data
    }

    /// A file of `size` bytes that takes no disk space.
    private func sparse(_ name: String, size: Int) throws -> URL {
        let url = dir.appendingPathComponent(name)
        FileManager.default.createFile(atPath: url.path, contents: nil)
        let h = try FileHandle(forWritingTo: url)
        try h.truncate(atOffset: UInt64(size))
        try h.close()
        return url
    }

    func testAFileAndAnImageAttach() async throws {
        let log = write("build.txt", Data("line one\n".utf8))
        let shot = write("shot.png", png(width: 3000, height: 1000))
        let out = await ChatDrop.prepare([.file(log), .file(shot)])
        XCTAssertEqual(out.attachments.map(\.name), ["build.txt", "shot.png"])
        XCTAssertEqual(out.attachments[0].type, "text/plain")
        XCTAssertEqual(out.attachments[0].data, Data("line one\n".utf8))
        // The image is prepared as a picked one: scaled to 2048 px on its long side, still PNG.
        let img = out.attachments[1]
        XCTAssertEqual(img.type, "image/png")
        let src = try XCTUnwrap(CGImageSourceCreateWithData(img.data as CFData, nil))
        let props = try XCTUnwrap(CGImageSourceCopyPropertiesAtIndex(src, 0, nil) as? [CFString: Any])
        XCTAssertEqual(props[kCGImagePropertyPixelWidth] as? Int, AttachmentLimits.maxSide)
        XCTAssertNil(out.text)
        XCTAssertTrue(out.notices.isEmpty)
        XCTAssertNil(out.notice)
    }

    func testAFolderAndAnOversizedFileAreRefused() async throws {
        let folder = dir.appendingPathComponent("photos")
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        let big = try sparse("disk.img", size: AttachmentLimits.maxBytes + 1)
        let ok = write("notes.txt", Data("hi".utf8))
        let out = await ChatDrop.prepare([.file(folder), .file(big), .file(ok)])
        XCTAssertEqual(out.attachments.map(\.name), ["notes.txt"])
        XCTAssertEqual(out.notices, ["photos is a folder; attach the files in it", "disk.img is over the 25 MB limit"])
        XCTAssertEqual(out.notice, "photos is a folder; attach the files in it (1 more not attached)")
    }

    func testAMissingFileIsRefused() async {
        let gone = dir.appendingPathComponent("gone.pdf")
        let out = await ChatDrop.prepare([.file(gone)])
        XCTAssertTrue(out.attachments.isEmpty)
        XCTAssertEqual(out.notices, ["Couldn't read gone.pdf"])
    }

    func testTextAndLinksGoToTheDraft() async {
        let out = await ChatDrop.prepare([.text("  see this  "), .text(""), .text("https://example.com/a")])
        XCTAssertTrue(out.attachments.isEmpty)
        XCTAssertEqual(out.text, "see this\nhttps://example.com/a")
        XCTAssertEqual(ChatDrop.draft("", adding: out.text), "see this\nhttps://example.com/a")
        XCTAssertEqual(ChatDrop.draft("look:", adding: "x"), "look:\nx")
        XCTAssertEqual(ChatDrop.draft("look:", adding: nil), "look:")
    }

    /// A file the system handed over (iPad) is copied, keeping its name, and read only when
    /// the drop lands; a folder or a file over the limit is refused before any copy.
    func testHandedOverFilesAreCopiedAndReadOnLanding() async throws {
        let photo = write("photo.jpg", Data([1, 2, 3]))
        let copy = ChatDropItem.copying(photo)
        guard case .copy(let url) = copy else { return XCTFail("a copy") }
        XCTAssertEqual(url.lastPathComponent, "photo.jpg")
        XCTAssertNotEqual(url, photo)
        let big = try sparse("movie.mov", size: AttachmentLimits.maxBytes + 1)
        XCTAssertEqual(ChatDropItem.copying(big), .refused("movie.mov is over the 25 MB limit"))
        let out = await ChatDrop.prepare([copy, ChatDropItem.copying(big)])
        XCTAssertEqual(out.attachments.map(\.name), ["photo.jpg"])
        XCTAssertEqual(out.attachments.first?.data, Data([1, 2, 3]))
        XCTAssertEqual(out.notices, ["movie.mov is over the 25 MB limit"])
        XCTAssertFalse(FileManager.default.fileExists(atPath: url.path), "the copy is deleted")
        XCTAssertFalse(FileManager.default.fileExists(atPath: url.deletingLastPathComponent().path))
        XCTAssertTrue(FileManager.default.fileExists(atPath: photo.path), "the original stays")
    }

    /// A copy past the cap is never read, and every copy is deleted.
    func testCopiesPastTheCapAreNotRead() async throws {
        var items = (1...10).map { ChatDropItem.copying(write("f\($0).txt", Data("\($0)".utf8))) }
        // Past the cap: unreadable once copied. Reading it would refuse it; the cap notice
        // shows it was never read.
        let late = ChatDropItem.copying(write("late.txt", Data("x".utf8)))
        guard case .copy(let lateURL) = late else { return XCTFail("a copy") }
        try FileManager.default.setAttributes([.posixPermissions: 0], ofItemAtPath: lateURL.path)
        items.append(late)
        let out = await ChatDrop.prepare(items)
        XCTAssertEqual(out.attachments.count, 10)
        XCTAssertEqual(out.notices, ["late.txt was not attached: at most 10 files in a message"])
        for case .copy(let u) in items {
            XCTAssertFalse(FileManager.default.fileExists(atPath: u.path), u.lastPathComponent)
        }
    }

    /// A refused drop deletes its copies.
    func testDiscardDeletesCopies() {
        let item = ChatDropItem.copying(write("a.txt", Data("a".utf8)))
        guard case .copy(let u) = item else { return XCTFail("a copy") }
        ChatDrop.discard([item, .text("x")])
        XCTAssertFalse(FileManager.default.fileExists(atPath: u.path))
    }

    /// Dropped text over 100 KB is refused, not put in the draft.
    func testLongTextIsRefused() async {
        let long = String(repeating: "a", count: ChatDrop.maxTextBytes + 1)
        let out = await ChatDrop.prepare([.text("short"), .text(long)])
        XCTAssertEqual(out.text, "short")
        XCTAssertEqual(out.notices, ["The dropped text is over 100 KB; attach it as a file"])
        let fits = String(repeating: "b", count: ChatDrop.maxTextBytes)
        let ok = await ChatDrop.prepare([.text(fits)])
        XCTAssertEqual(ok.text?.utf8.count, ChatDrop.maxTextBytes)
    }

    /// The cap counts the files the composer holds already.
    func testTheCapCountsHeldFiles() async {
        let urls = (1...3).map { write("g\($0).txt", Data("\($0)".utf8)) }
        let out = await ChatDrop.prepare(urls.map(ChatDropItem.file), holding: 8)
        XCTAssertEqual(out.attachments.map(\.name), ["g1.txt", "g2.txt"])
        XCTAssertEqual(out.notices, ["g3.txt was not attached: at most 10 files in a message"])
    }

    /// The outcome lands only while the composer still takes files, within the cap as it is
    /// when the drop lands.
    func testLandingChecksTheComposerAgain() {
        let a = DraftAttachment(name: "a.txt", type: "text/plain", data: Data("a".utf8))
        let b = DraftAttachment(name: "b.txt", type: "text/plain", data: Data("b".utf8))
        let out = ChatDrop.Outcome(attachments: [a, b], text: "see", notices: [])
        var draft = "hi"
        var held: [DraftAttachment] = []
        // The composer switched to answer a question while the drop was read.
        XCTAssertEqual(ChatDrop.land(out, takesFiles: false, draft: &draft, attachments: &held),
                       "The drop was not added: the composer takes no files now")
        XCTAssertEqual(draft, "hi")
        XCTAssertTrue(held.isEmpty)
        // Another drop filled the composer to nine meanwhile.
        held = Array(repeating: a, count: 9)
        XCTAssertEqual(ChatDrop.land(out, takesFiles: true, draft: &draft, attachments: &held),
                       "b.txt was not attached: at most 10 files in a message")
        XCTAssertEqual(held.count, 10)
        XCTAssertEqual(draft, "hi\nsee")
    }

    /// The drop follows the attach button: off while the composer is off or answers a question.
    func testTakesFilesFollowsTheComposer() {
        XCTAssertTrue(ChatDrop.takesFiles(.message))
        XCTAssertTrue(ChatDrop.takesFiles(.queue))
        XCTAssertFalse(ChatDrop.takesFiles(.answer))
        XCTAssertFalse(ChatDrop.takesFiles(.off))
    }

    /// A web link dropped as a URL is text; a file URL is a file.
    func testURLsSplitIntoFilesAndText() {
        let file = write("a.txt", Data("a".utf8))
        let fromFile = ChatDropItem.of(file)
        let fromWeb = ChatDropItem.of(URL(string: "https://example.com/x")!)
        guard case .file(let u) = fromFile else { return XCTFail("a file URL is a file") }
        XCTAssertEqual(u, file)
        guard case .text(let t) = fromWeb else { return XCTFail("a web link is text") }
        XCTAssertEqual(t, "https://example.com/x")
    }
}

