import CoreGraphics
import Foundation
import HabAPI
import ImageIO

/// Loaded file bytes for the chat view (app.md 4.7): keyed by item and file version
/// (size and modification time), bounded by total bytes, the least recently used out first.
final class ByteCache {
    /// A file version: what the list said its size and modification time were.
    struct Key: Hashable, Sendable {
        var id: String
        var size: Int?
        var modified: String?

        init(id: String, size: Int?, modified: String?) {
            self.id = id
            self.size = size
            self.modified = modified
        }

        init(_ it: ContentItem) {
            self.init(id: it.id, size: it.size, modified: it.modified)
        }
    }

    let limit: Int
    private(set) var total = 0
    private var entries: [String: (key: Key, data: Data)] = [:]
    /// Item ids, least recently used first.
    private var order: [String] = []

    init(limit: Int) {
        self.limit = limit
    }

    /// The bytes held for this version, or nil when none are, or they belong to another version.
    func get(id: String, key: Key) -> Data? {
        guard let e = entries[id], e.key == key else { return nil }
        touch(id)
        return e.data
    }

    /// Holds bytes for one version, replacing any other version of the item. Bytes larger
    /// than the whole limit are not held.
    func put(id: String, key: Key, data: Data) {
        drop(id)
        guard data.count <= limit else { return }
        entries[id] = (key, data)
        order.append(id)
        total += data.count
        while total > limit, let oldest = order.first {
            drop(oldest)
        }
    }

    /// Drops an item's bytes unless they are of this version.
    func keep(id: String, key: Key) {
        if let e = entries[id], e.key != key { drop(id) }
    }

    func drop(_ id: String) {
        guard let e = entries.removeValue(forKey: id) else { return }
        total -= e.data.count
        order.removeAll { $0 == id }
    }

    private func touch(_ id: String) {
        order.removeAll { $0 == id }
        order.append(id)
    }

    var count: Int { entries.count }
}

/// Decoded images for the chat view (app.md 4.7), bounded by their bitmap bytes first and by
/// count second, the least recently used out first. The newest image always stays, even past
/// the byte bound, so a view never decodes it again on each draw.
final class ImageCache {
    /// The one cache every open chat shares, so the byte bound holds for the device. It
    /// empties when the system reports memory pressure.
    @MainActor static let shared: ImageCache = {
        let c = ImageCache()
        let src = DispatchSource.makeMemoryPressureSource(eventMask: [.warning, .critical], queue: .main)
        src.setEventHandler { MainActor.assumeIsolated { c.removeAll() } }
        src.activate()
        c.pressure = src
        return c
    }()

    private var pressure: DispatchSourceMemoryPressure?

    func removeAll() {
        entries.removeAll()
        order.removeAll()
        total = 0
    }

    let byteLimit: Int
    let countLimit: Int
    private(set) var total = 0
    private var entries: [String: CGImage] = [:]
    /// Keys, least recently used first.
    private var order: [String] = []

    init(byteLimit: Int = ImageCache.defaultByteLimit, countLimit: Int = 24) {
        self.byteLimit = byteLimit
        self.countLimit = countLimit
    }

    /// A 32nd of the device's memory, from 64 to 256 MiB: 128 MiB on a 4 GiB phone, room for
    /// two 4096 px zoomed images and the inline ones.
    static let defaultByteLimit = budget(physicalMemory: ProcessInfo.processInfo.physicalMemory)

    static func budget(physicalMemory: UInt64) -> Int {
        Int(min(max(physicalMemory / 32, 64 << 20), 256 << 20))
    }

    /// The decoded size of an image.
    static func cost(_ img: CGImage) -> Int { img.bytesPerRow * img.height }

    func get(_ key: String) -> CGImage? {
        guard let img = entries[key] else { return nil }
        order.removeAll { $0 == key }
        order.append(key)
        return img
    }

    func put(_ key: String, _ img: CGImage) {
        if let old = entries.removeValue(forKey: key) {
            total -= Self.cost(old)
            order.removeAll { $0 == key }
        }
        entries[key] = img
        order.append(key)
        total += Self.cost(img)
        while order.count > 1, total > byteLimit || order.count > countLimit {
            let oldest = order.removeFirst()
            if let e = entries.removeValue(forKey: oldest) { total -= Self.cost(e) }
        }
    }

    var count: Int { entries.count }
}

/// Image decoding that never builds the full-size bitmap.
enum ImageDecode {
    /// The image scaled so its long side is at most `maxPixel`, with its orientation applied.
    static func thumbnail(_ data: Data, maxPixel: Int) -> CGImage? {
        guard let src = CGImageSourceCreateWithData(data as CFData, [kCGImageSourceShouldCache: false] as CFDictionary) else { return nil }
        let opts: [CFString: Any] = [
            kCGImageSourceCreateThumbnailFromImageAlways: true,
            kCGImageSourceCreateThumbnailWithTransform: true,
            kCGImageSourceShouldCacheImmediately: true,
            kCGImageSourceThumbnailMaxPixelSize: maxPixel,
        ]
        return CGImageSourceCreateThumbnailAtIndex(src, 0, opts as CFDictionary)
    }
}

/// Text a document card can render without holding up the main thread.
enum ContentText {
    /// The most text one view renders at once.
    static let previewLimit = 256 << 10

    /// The first `limit` bytes as text, cut at a line end, and whether more follows.
    static func preview(_ data: Data, limit: Int = previewLimit) -> (text: String, truncated: Bool) {
        guard data.count > limit else { return (String(decoding: data, as: UTF8.self), false) }
        var cut = data.prefix(limit)
        if let nl = cut.lastIndex(of: 0x0A) { cut = cut[..<nl] }
        return (String(decoding: cut, as: UTF8.self), true)
    }

    /// The whole text in runs of `lines` lines, for a lazy list.
    static func chunks(_ data: Data, lines: Int = 200) -> [String] {
        let all = String(decoding: data, as: UTF8.self).components(separatedBy: "\n")
        return stride(from: 0, to: all.count, by: lines).map { all[$0..<min($0 + lines, all.count)].joined(separator: "\n") }
    }
}
