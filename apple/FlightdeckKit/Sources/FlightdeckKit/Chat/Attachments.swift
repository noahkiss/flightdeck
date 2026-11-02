import CoreGraphics
import CryptoKit
import Foundation
import HabAPI
import ImageIO
import UniformTypeIdentifiers

/// A file the owner attached in the composer and has not sent yet (app.md 4.4).
public struct DraftAttachment: Identifiable, Equatable, Sendable {
    public var id = UUID()
    /// The file name as the owner sees it.
    public var name: String
    /// Media type, such as image/png.
    public var type: String
    public var data: Data
    /// Counts changes to `data` (markup), so a preview made from it is made again.
    public var version = 0

    public init(name: String, type: String, data: Data) {
        self.name = name
        self.type = type
        self.data = data
    }

    public var isImage: Bool { type.hasPrefix("image/") }
}

/// The attachment limits (app.md 4.4).
public enum AttachmentLimits {
    /// The largest file the app sends: the hab's limit.
    public static let maxBytes = 25_000_000
    /// A photo's longest side after downscaling, in pixels.
    public static let maxSide = 2048
    /// The hab capability that says `attachment.*` works.
    public static let capability = "attachments"

    /// Why a file cannot be sent, or nil when it can.
    public static func refusal(_ a: DraftAttachment) -> String? {
        a.data.count > maxBytes ? "\(a.name) is over the 25 MB limit" : nil
    }
}

/// A stored attachment on the hab.
public struct UploadedAttachment: Equatable, Sendable {
    public var id: String
    public var path: String
}

/// Uploads one file: `attachment.put`, then `attachment.write` runs of the size the hab names,
/// in order (`hab-logic-api.md`, `attachment.*`).
public enum AttachmentUpload {
    /// A run when the hab names none.
    static let defaultChunk = 1 << 20
    /// The most bytes one `attachment.write` carries, whatever the hab allows (app.md 4.4):
    /// each run's reply keeps the connection's liveness check fed over a slow uplink.
    static let maxWrite = 256 << 10

    public static func sha256(_ data: Data) -> String {
        SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }

    /// Uploads `a` for `session`. `progress` hears the bytes the hab holds.
    public static func run(_ a: DraftAttachment, session: String, connection: HabConnection,
                           progress: (@MainActor (Int) -> Void)? = nil) async throws -> UploadedAttachment {
        let put = try await connection.call(HabMethods.attachmentPut, AttachmentPutParams(
            name: a.name, session: session, sha256: sha256(a.data), size: UInt64(a.data.count), type: a.type))
        if put.done == true || a.data.isEmpty {
            return UploadedAttachment(id: put.id, path: put.path)
        }
        let chunk = max(1, min(put.chunk ?? maxWrite, maxWrite))
        var offset = 0
        while offset < a.data.count {
            let end = min(offset + chunk, a.data.count)
            let w = try await connection.call(HabMethods.attachmentWrite, AttachmentWriteParams(
                data: a.data.subdata(in: offset..<end), id: put.id, offset: offset))
            // A hab that takes a run but holds no more bytes would loop forever.
            guard w.done || w.received > offset else { throw UploadStalled(id: put.id, received: w.received) }
            offset = w.received
            await progress?(offset)
            if w.done { break }
        }
        return UploadedAttachment(id: put.id, path: put.path)
    }

    /// The whole of a stored file, read in runs.
    public static func fetch(_ id: String, connection: HabConnection) async throws -> AttachmentData {
        var first = try await connection.call(HabMethods.attachmentGet, AttachmentGetParams(id: id, limit: defaultChunk))
        var data = first.data
        while first.eof != true, !first.data.isEmpty, data.count < AttachmentLimits.maxBytes {
            let next = try await connection.call(HabMethods.attachmentGet, AttachmentGetParams(id: id, limit: defaultChunk, offset: data.count))
            data.append(next.data)
            first.eof = next.eof
            if next.data.isEmpty { break }
        }
        first.data = data
        return first
    }
}

/// The hab took a run but its count of bytes did not grow.
public struct UploadStalled: Error, CustomStringConvertible {
    public var id: String
    public var received: Int
    public var description: String { "upload \(id) stopped at \(received) bytes" }
}

/// Small previews of composer attachments, made once per file version and kept: a view body
/// asks for them on every pass, and an upload changes the row every 256 KiB.
@MainActor
enum DraftThumbs {
    private static let cache: NSCache<NSString, CGImage> = {
        let c = NSCache<NSString, CGImage>()
        c.countLimit = 64
        return c
    }()

    /// The preview of `a`, or nil when it is no image this device can read.
    static func image(_ a: DraftAttachment) -> CGImage? {
        guard a.isImage else { return nil }
        let key = "\(a.id.uuidString)-\(a.version)" as NSString
        if let img = cache.object(forKey: key) { return img }
        guard let img = ImageDecode.thumbnail(a.data, maxPixel: 192) else { return nil }
        cache.setObject(img, forKey: key)
        return img
    }
}

/// The text the hab delivers for a message with attachments: the text, a blank line, then each
/// path (`harness-adapters.md`, Input delivery). The app types it this way only where the hab
/// has no adapter for the harness.
public enum AttachmentText {
    public static func message(_ text: String, paths: [String]) -> String {
        guard !paths.isEmpty else { return text }
        let list = paths.joined(separator: "\n")
        let t = text.trimmingCharacters(in: .whitespacesAndNewlines)
        return t.isEmpty ? list : t + "\n\n" + list
    }
}

/// Prepares a picked or pasted image for upload (app.md 4.4): scaled to `maxSide` on its long
/// side, its orientation applied, and written again with no metadata, so no location or camera
/// data leaves the phone. A screenshot (a PNG source) stays PNG; a photo becomes JPEG.
public enum AttachmentPrep {
    /// The prepared image, or nil when the data is no image this device can read.
    public static func image(_ data: Data, name: String, maxSide: Int = AttachmentLimits.maxSide) -> DraftAttachment? {
        guard let src = CGImageSourceCreateWithData(data as CFData, [kCGImageSourceShouldCache: false] as CFDictionary),
              let props = CGImageSourceCopyPropertiesAtIndex(src, 0, nil) as? [CFString: Any] else { return nil }
        let w = props[kCGImagePropertyPixelWidth] as? Int ?? 0
        let h = props[kCGImagePropertyPixelHeight] as? Int ?? 0
        guard w > 0, h > 0 else { return nil }
        let srcType = CGImageSourceGetType(src) as String? ?? ""
        let png = srcType == UTType.png.identifier || (props[kCGImagePropertyHasAlpha] as? Bool ?? false)
        let opts: [CFString: Any] = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceShouldCacheImmediately: true,
            kCGImageSourceThumbnailMaxPixelSize: min(max(w, h), maxSide),
        ]
        guard let img = CGImageSourceCreateThumbnailAtIndex(src, 0, opts as CFDictionary) else { return nil }
        guard let out = encode(img, png: png) else { return nil }
        let stem = (name as NSString).deletingPathExtension
        return DraftAttachment(name: (stem.isEmpty ? "image" : stem) + (png ? ".png" : ".jpg"), type: png ? "image/png" : "image/jpeg", data: out)
    }

    /// Writes an image as PNG or JPEG with no metadata.
    public static func encode(_ img: CGImage, png: Bool) -> Data? {
        let out = NSMutableData()
        let type = (png ? UTType.png : UTType.jpeg).identifier as CFString
        guard let dst = CGImageDestinationCreateWithData(out, type, 1, nil) else { return nil }
        let props: [CFString: Any] = png ? [:] : [kCGImageDestinationLossyCompressionQuality: 0.85]
        CGImageDestinationAddImage(dst, img, props as CFDictionary)
        guard CGImageDestinationFinalize(dst) else { return nil }
        return out as Data
    }

    /// A file as an attachment: images are prepared as above; other files (a video, a PDF)
    /// go as they are, metadata and all, with their media type from the name.
    public static func file(_ data: Data, name: String) -> DraftAttachment {
        let type = UTType(filenameExtension: (name as NSString).pathExtension)
        if type?.conforms(to: .image) == true, let img = image(data, name: name) { return img }
        return DraftAttachment(name: name.isEmpty ? "file" : name, type: type?.preferredMIMEType ?? "application/octet-stream", data: data)
    }
}

/// Thumbnails and full images of the attachments on chat messages, from the hab's copies
/// (`attachment.get`), so every device shows them.
@MainActor
@Observable
public final class AttachmentImages {
    @ObservationIgnored private let connection: HabConnection?
    private(set) var thumbs: [String: CGImage] = [:]
    /// Name and size of a file with no thumbnail, for its chip.
    private(set) var files: [String: FileInfo] = [:]
    private(set) var failed: Set<String> = []
    /// Thumbnails that kept failing for a passing reason (a dropped connection): the row offers
    /// a retry instead of a spinner.
    private(set) var stalled: Set<String> = []
    @ObservationIgnored private var loading: Set<String> = []
    /// The waits between tries after a passing failure.
    @ObservationIgnored var retryDelays: [Duration] = [.seconds(1), .seconds(3), .seconds(8)]

    struct FileInfo: Equatable {
        var name: String
        var size: Int?
        /// The hab no longer has the file.
        var gone = false
    }

    init(connection: HabConnection?) {
        self.connection = connection
    }

    func thumbnail(_ id: String) -> CGImage? { thumbs[id] }
    func didFail(_ id: String) -> Bool { failed.contains(id) }
    func file(_ id: String) -> FileInfo? { files[id] }
    func isStalled(_ id: String) -> Bool { stalled.contains(id) }

    /// Tries a stalled thumbnail again, from the row's retry.
    func retry(_ id: String) async {
        stalled.remove(id)
        await load(id)
    }

    /// Loads a thumbnail once. A file with no thumbnail (not an image) gets its name and size
    /// for a chip; a file the hab no longer has says so. Any other error (a dropped connection)
    /// is tried again after each of `retryDelays`, then the row offers a retry.
    func load(_ id: String) async {
        guard thumbs[id] == nil, !failed.contains(id), !stalled.contains(id), !loading.contains(id), connection != nil else { return }
        loading.insert(id)
        defer { loading.remove(id) }
        var waits = retryDelays[...]
        while await !tryLoad(id) {
            guard let w = waits.popFirst() else {
                stalled.insert(id)
                return
            }
            do { try await Task.sleep(for: w) } catch { return } // the row went away
        }
    }

    /// One try. True when it settled the id: a thumbnail, a chip, or gone.
    private func tryLoad(_ id: String) async -> Bool {
        guard let c = connection else { return true }
        do {
            let d = try await c.call(HabMethods.attachmentGet, AttachmentGetParams(id: id, thumbnail: true))
            if let img = ImageDecode.thumbnail(d.data, maxPixel: 640) {
                thumbs[id] = img
            } else {
                files[id] = FileInfo(name: d.name, size: nil)
                failed.insert(id)
            }
        } catch let e as RPCError where e.kind == .invalid {
            // No thumbnail: one byte of the file brings its name and size.
            if let d = try? await c.call(HabMethods.attachmentGet, AttachmentGetParams(id: id, limit: 1)) {
                files[id] = FileInfo(name: d.name, size: d.size)
            }
            failed.insert(id)
        } catch let e as RPCError where e.kind == .notFound {
            files[id] = FileInfo(name: "File", size: nil, gone: true)
            failed.insert(id)
        } catch {
            return false // passing: the caller waits and tries again
        }
        return true
    }

    /// The full image, for the zoomed view.
    func full(_ id: String) async -> CGImage? {
        guard let c = connection, let d = try? await AttachmentUpload.fetch(id, connection: c) else { return nil }
        return ImageDecode.thumbnail(d.data, maxPixel: 4096)
    }
}
