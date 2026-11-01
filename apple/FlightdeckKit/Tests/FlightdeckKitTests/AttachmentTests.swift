import CoreGraphics
import Foundation
import HabAPI
import ImageIO
import UniformTypeIdentifiers
import XCTest
@testable import FlightdeckKit

@MainActor
final class AttachmentTests: XCTestCase {
    /// A fleet with one fake hab whose session s1 (pane %1) runs claude, idle, with attachments on.
    private func setup(attachments: Bool = true) async -> (Fleet, FakeHab, ChatModel) {
        let hab = await sampleHab()
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .idle))
        await hab.setChat(session: "s1", [])
        await hab.setCapability("attachments", attachments)
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await fleet.setActive(true)
        await eventually("online") { fleet.board.habs.first?.state.status == .online }
        let model = fleet.chat(for: SessionTarget(habID: "h", session: "s1", pane: "%1"))
        model.readBackWindow = .milliseconds(300)
        return (fleet, hab, model)
    }

    private func png(width: Int, height: Int, alpha: Bool = false) -> Data {
        let ctx = CGContext(data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
                            space: CGColorSpace(name: CGColorSpace.sRGB)!,
                            bitmapInfo: (alpha ? CGImageAlphaInfo.premultipliedLast : CGImageAlphaInfo.noneSkipLast).rawValue)!
        ctx.setFillColor(CGColor(red: 1, green: 0, blue: 0, alpha: 1))
        ctx.fill(CGRect(x: 0, y: 0, width: width / 2, height: height))
        let out = NSMutableData()
        let dst = CGImageDestinationCreateWithData(out, UTType.png.identifier as CFString, 1, nil)!
        CGImageDestinationAddImage(dst, ctx.makeImage()!, nil)
        CGImageDestinationFinalize(dst)
        return out as Data
    }

    /// A JPEG with a GPS block, as a phone camera writes one.
    private func photoWithLocation(width: Int, height: Int) -> Data {
        let ctx = CGContext(data: nil, width: width, height: height, bitsPerComponent: 8, bytesPerRow: 0,
                            space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue)!
        let out = NSMutableData()
        let dst = CGImageDestinationCreateWithData(out, UTType.jpeg.identifier as CFString, 1, nil)!
        let gps: [CFString: Any] = [kCGImagePropertyGPSLatitude: 12.5, kCGImagePropertyGPSLatitudeRef: "N",
                                    kCGImagePropertyGPSLongitude: 45.25, kCGImagePropertyGPSLongitudeRef: "E"]
        CGImageDestinationAddImage(dst, ctx.makeImage()!, [kCGImagePropertyGPSDictionary: gps, kCGImagePropertyOrientation: 1] as CFDictionary)
        CGImageDestinationFinalize(dst)
        return out as Data
    }

    private func props(_ data: Data) -> [CFString: Any] {
        let src = CGImageSourceCreateWithData(data as CFData, nil)!
        return CGImageSourceCopyPropertiesAtIndex(src, 0, nil) as! [CFString: Any]
    }

    /// A message with two files uploads each in runs of the size the hab names, then sends the
    /// text with both ids in order; the outgoing row shows the upload, then Sent.
    func testSendUploadsInRunsThenSendsIDs() async {
        let (fleet, hab, model) = await setup()
        await hab.setAttachmentChunk(1000)
        let a = DraftAttachment(name: "shot.png", type: "image/png", data: Data(repeating: 7, count: 2500))
        let b = DraftAttachment(name: "log.txt", type: "text/plain", data: Data("hello".utf8))
        model.send("look at this", attachments: [a, b])
        await eventually("sent") { model.outgoing.first?.status == .sent }
        let sends = await hab.sends
        XCTAssertEqual(sends.count, 1)
        XCTAssertEqual(sends.first?.text, "look at this")
        XCTAssertEqual(sends.first?.attachments?.count, 2)
        let stored = await hab.storedFiles
        XCTAssertEqual(stored[sends[0].attachments![0]]?.data, a.data)
        XCTAssertEqual(stored[sends[0].attachments![1]]?.data, b.data)
        let writes = await hab.attachmentWrites
        XCTAssertEqual(writes, 4, "2500 bytes in 1000-byte runs, then one run")
        XCTAssertEqual(model.outgoing.first?.uploaded.count, 2)
        await fleet.setActive(false)
    }

    /// Files alone, with no text, are a message.
    func testFilesWithNoText() async {
        let (fleet, hab, model) = await setup()
        model.send("  ", attachments: [DraftAttachment(name: "a.png", type: "image/png", data: Data([1, 2, 3]))])
        await eventually("sent") { model.outgoing.first?.status == .sent }
        let sends = await hab.sends
        XCTAssertEqual(sends.first?.text, "")
        XCTAssertEqual(sends.first?.attachments?.count, 1)
        await fleet.setActive(false)
    }

    /// A broken upload leaves the message "not sent" with a retry; the retry uploads only the
    /// files the hab does not hold, then sends.
    func testRetryAfterABrokenUploadUploadsOnlyTheRest() async {
        let a = DraftAttachment(name: "a.png", type: "image/png", data: Data([1]))
        let (fleet, hab, model) = await setup()
        await hab.setAttachmentChunk(1)
        model.send("two", attachments: [a, DraftAttachment(name: "b.png", type: "image/png", data: Data(count: 2000))])
        // The second file goes up one byte per write: the failures start long before its end.
        await eventually("first file stored") { await hab.storedFiles.count == 1 }
        await hab.failNextAttachmentWrites(5)
        await eventually("not sent") { if case .failed = model.outgoing.first?.status { return true }; return false }
        XCTAssertEqual(model.outgoing.first?.uploaded.count, 1)
        await hab.failNextAttachmentWrites(0)
        model.retry(model.outgoing[0].id)
        await eventually("sent after retry") { model.outgoing.first?.status == .sent }
        let stored = await hab.storedFiles.count
        XCTAssertEqual(stored, 2, "the first file was not uploaded twice")
        let sends = await hab.sends
        XCTAssertEqual(sends.first?.attachments?.count, 2)
        await fleet.setActive(false)
    }

    /// A hab with no attachments capability is refused before any upload, with a reason.
    func testHabWithoutTheCapability() async {
        let (fleet, hab, model) = await setup(attachments: false)
        model.send("x", attachments: [DraftAttachment(name: "a.png", type: "image/png", data: Data([1]))])
        await eventually("failed") { if case .failed = model.outgoing.first?.status { return true }; return false }
        guard case .failed(let why) = model.outgoing.first?.status else { return XCTFail() }
        XCTAssertTrue(why.contains("cannot take files"), why)
        let writes = await hab.attachmentWrites
        XCTAssertEqual(writes, 0)
        await fleet.setActive(false)
    }

    /// A file over 25 MB is refused before upload and the limit is named.
    func testOverTheLimitIsRefusedBeforeUpload() async {
        let (fleet, _, model) = await setup()
        model.send("big", attachments: [DraftAttachment(name: "big.mov", type: "video/quicktime", data: Data(count: AttachmentLimits.maxBytes + 1))])
        XCTAssertTrue(model.outgoing.isEmpty)
        XCTAssertEqual(model.notice, "big.mov is over the 25 MB limit")
        await fleet.setActive(false)
    }

    /// A user message's attachment ids become a row with ids; thumbnails load from the hab.
    func testChatRowShowsAttachmentIDsAndLoadsThumbnails() async {
        let (fleet, hab, model) = await setup()
        let image = png(width: 40, height: 20)
        await hab.storeAttachment(id: "0123456789ab", name: "shot.png", data: image)
        await hab.putChat(ChatMessage(at: "2026-01-01T10:00:00Z", attachments: ["0123456789ab"], id: "m1", kind: .user, session: "s1", text: "see"))
        model.start()
        await eventually("row") { !model.items.isEmpty }
        guard case .user(_, let text, _, let ids) = model.items[0] else { return XCTFail("no user row") }
        XCTAssertEqual(text, "see")
        XCTAssertEqual(ids, ["0123456789ab"])
        await model.images.load("0123456789ab")
        XCTAssertEqual(model.images.thumbnail("0123456789ab")?.width, 40)
        await model.images.load("ffffffffffff")
        XCTAssertTrue(model.images.didFail("ffffffffffff"))
        let full = await model.images.full("0123456789ab")
        XCTAssertEqual(full?.height, 20)
        model.stop()
        await fleet.setActive(false)
    }

    /// A large photo is scaled to 2048 px on its long side, written as JPEG with no location;
    /// a screenshot (PNG) stays PNG.
    func testPrepScalesStripsAndKeepsScreenshotsPNG() {
        let photo = photoWithLocation(width: 4000, height: 3000)
        XCTAssertNotNil(props(photo)[kCGImagePropertyGPSDictionary], "the fixture carries a location")
        let p = AttachmentPrep.image(photo, name: "IMG_0001.HEIC")!
        XCTAssertEqual(p.type, "image/jpeg")
        XCTAssertEqual(p.name, "IMG_0001.jpg")
        let pp = props(p.data)
        XCTAssertEqual(pp[kCGImagePropertyPixelWidth] as? Int, 2048)
        XCTAssertEqual(pp[kCGImagePropertyPixelHeight] as? Int, 1536)
        XCTAssertNil(pp[kCGImagePropertyGPSDictionary], "the location left the phone")

        let shot = AttachmentPrep.image(png(width: 300, height: 600), name: "Screenshot.png")!
        XCTAssertEqual(shot.type, "image/png")
        XCTAssertEqual(props(shot.data)[kCGImagePropertyPixelHeight] as? Int, 600, "a small image is not enlarged or shrunk")

        XCTAssertNil(AttachmentPrep.image(Data("not an image".utf8), name: "x.png"))
        let text = AttachmentPrep.file(Data("log".utf8), name: "build.log")
        XCTAssertEqual(text.name, "build.log")
        XCTAssertFalse(text.isImage)
    }

    /// The text typed where the hab has no adapter: the text, a blank line, the paths.
    func testAttachmentText() {
        XCTAssertEqual(AttachmentText.message("look", paths: ["/p/a.png", "/p/b.png"]), "look\n\n/p/a.png\n/p/b.png")
        XCTAssertEqual(AttachmentText.message(" ", paths: ["/p/a.png"]), "/p/a.png")
        XCTAssertEqual(AttachmentText.message("plain", paths: []), "plain")
    }

    /// The upload text names the file in progress and its percent.
    func testUploadText() {
        var o = Outgoing(id: 1, text: "", status: .sending)
        o.attachments = [DraftAttachment(name: "a", type: "image/png", data: Data(count: 200)),
                         DraftAttachment(name: "b", type: "image/png", data: Data(count: 100))]
        o.uploadedBytes = 50
        XCTAssertEqual(OutgoingView.uploadText(o), "Uploading 1 of 2, 25%")
        o.uploaded = [UploadedAttachment(id: "x", path: "/p")]
        o.uploadedBytes = 100
        XCTAssertEqual(OutgoingView.uploadText(o), "Uploading 2 of 2, 100%")
        o.uploaded.append(UploadedAttachment(id: "y", path: "/q"))
        XCTAssertNil(OutgoingView.uploadText(o))
    }

    /// Markup: the canvas sits on the image's fitted frame, and the marks land on the image at
    /// full size.
    func testMarkupGeometryAndCompose() {
        XCTAssertEqual(Markup.fit(CGSize(width: 200, height: 100), in: CGRect(x: 0, y: 0, width: 100, height: 100)),
                       CGRect(x: 0, y: 25, width: 100, height: 50))
        let base = AttachmentPrep.image(png(width: 40, height: 20), name: "a.png")!
        let src = CGImageSourceCreateWithData(base.data as CFData, nil)!
        let img = CGImageSourceCreateImageAtIndex(src, 0, nil)!
        // Marks drawn at a quarter of the size: a blue band over the right half.
        let m = CGContext(data: nil, width: 10, height: 5, bitsPerComponent: 8, bytesPerRow: 0,
                          space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
        m.setFillColor(CGColor(red: 0, green: 0, blue: 1, alpha: 1))
        m.fill(CGRect(x: 5, y: 0, width: 5, height: 5))
        let out = Markup.compose(img, marks: m.makeImage(), png: true)!
        XCTAssertEqual(props(out)[kCGImagePropertyPixelWidth] as? Int, 40)
        let o = CGImageSourceCreateImageAtIndex(CGImageSourceCreateWithData(out as CFData, nil)!, 0, nil)!
        let px = pixel(o, x: 35, y: 10)
        XCTAssertTrue(px.b > 200 && px.r < 50, "the mark is on the right half: \(px)")
        let left = pixel(o, x: 5, y: 10)
        XCTAssertTrue(left.r > 200 && left.b < 50, "the image shows on the left: \(left)")
    }

    private func pixel(_ img: CGImage, x: Int, y: Int) -> (r: Int, g: Int, b: Int) {
        let ctx = CGContext(data: nil, width: img.width, height: img.height, bitsPerComponent: 8, bytesPerRow: img.width * 4,
                            space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
        ctx.draw(img, in: CGRect(x: 0, y: 0, width: img.width, height: img.height))
        let p = ctx.data!.assumingMemoryBound(to: UInt8.self)
        let i = (y * img.width + x) * 4
        return (Int(p[i]), Int(p[i + 1]), Int(p[i + 2]))
    }

    // MARK: - Open in Flightdeck

    /// A private key goes to key import; a public key and a broken key block too, so key
    /// import explains them; any other file goes to the send sheet.
    func testRouteKeysToKeyImportAndOtherFilesToSend() {
        XCTAssertEqual(SendFileCenter.route(Data(KeyFixtures.ed25519.utf8)), .key)
        XCTAssertEqual(SendFileCenter.route(Data("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA== someone".utf8)), .key)
        XCTAssertEqual(SendFileCenter.route(Data("-----BEGIN OPENSSH PRIVATE KEY-----\nabc".utf8)), .key)
        XCTAssertEqual(SendFileCenter.route(Data("# Notes\nplain text".utf8)), .file)
        XCTAssertEqual(SendFileCenter.route(png(width: 4, height: 4)), .file)
        XCTAssertEqual(SendFileCenter.route(Data(count: 70_000)), .file)
    }

    /// An opened file waits in the send sheet; picking a session hands it to that session's
    /// composer once and asks the board to open it; a file over the limit shows the limit.
    func testOpenedFileGoesToThePickedSessionsComposer() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let url = dir.appendingPathComponent("trace.log")
        try Data("line 1\n".utf8).write(to: url)
        let center = SendFileCenter()
        XCTAssertTrue(center.open(url))
        XCTAssertEqual(center.incoming?.attachment?.name, "trace.log")
        XCTAssertNil(center.incoming?.error)
        let target = SessionTarget(habID: "h", session: "s1", pane: "%1")
        center.send(to: target)
        XCTAssertNil(center.incoming)
        XCTAssertEqual(center.openRequest, target)
        XCTAssertEqual(center.handed, 1)
        XCTAssertEqual(center.take(for: target).map(\.name), ["trace.log"])
        XCTAssertTrue(center.take(for: target).isEmpty, "taken once")

        let key = dir.appendingPathComponent("id_ed25519")
        try Data(KeyFixtures.ed25519.utf8).write(to: key)
        XCTAssertFalse(center.open(key), "a key goes to key import")
        XCTAssertNil(center.incoming)

        center.receive(Data(count: AttachmentLimits.maxBytes + 1), name: "huge.bin")
        XCTAssertEqual(center.incoming?.error, "huge.bin is over the 25 MB limit")
        XCTAssertNil(center.incoming?.attachment)
    }

    /// The send sheet lists sessions by host in board order and leaves out rows with no session.
    func testSendTargetsGroupByHost() {
        func row(_ id: String, host: String, session: String?) -> BoardRow {
            BoardRow(id: id, target: SessionTarget(habID: host, session: session, pane: nil), host: host, title: id, rank: .working)
        }
        let g = SendTargets.groups([row("a", host: "one", session: "s1"), row("b", host: "two", session: "s2"),
                                    row("c", host: "one", session: "s3"), row("d", host: "one", session: nil)])
        XCTAssertEqual(g.map(\.host), ["one", "two"])
        XCTAssertEqual(g[0].rows.map(\.id), ["a", "c"])
    }
}
