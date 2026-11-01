import CoreGraphics
import Foundation
import HabAPI
import Observation

/// What the app does with a content item's bytes (app.md 4.7).
public enum ContentBytes: Equatable, Sendable {
    case data(Data, changedWhileLoading: Bool)
    /// The hab is offline: a plain placeholder, not an error.
    case offline(String)
    /// The file is no longer on the host.
    case gone
    /// Over the hab's read limit.
    case tooLarge(Int)
    case failed(String)
}

/// A diff as the app shows it.
public enum ContentDiffLoad: Equatable, Sendable {
    case diff(ParsedDiff, source: String, truncated: Bool)
    case offline(String)
    case gone
    case failed(String)
}

/// Where content shows in the chat: under a row, or after the last row.
public struct ContentPlacement: Equatable, Sendable {
    /// Items under each row, by row id, in time order.
    public var byRow: [String: [ContentItem]] = [:]
    /// Shown items newer than every message, or whose message is not held.
    public var trailing: [ContentItem] = []

    public init() {}

    /// An item shows inline when the agent showed it, or when it is an image a
    /// tool call touched. It goes under its tool call when that row is held;
    /// a shown item without one goes under the last row at or before its time.
    public static func of(items: [ContentItem], messages: [ChatMessage], rowIDs: Set<String>) -> ContentPlacement {
        var p = ContentPlacement()
        let parent = Dictionary(messages.map { ($0.id, $0.parent) }, uniquingKeysWith: { a, _ in a })
        let rows = messages.filter { rowIDs.contains($0.id) }
        for it in items where it.shown == true || (it.kind == .image && it.message != nil) {
            if let m = it.message {
                // A nested call's row is its subagent's card.
                let row = rowIDs.contains(m) ? m : (parent[m] ?? nil).flatMap { rowIDs.contains($0) ? $0 : nil }
                if let row {
                    p.byRow[row, default: []].append(it)
                    continue
                }
            }
            guard it.shown == true else { continue }
            let at = ContentModel.date(it.at)
            if let at, let last = rows.last(where: { (ContentModel.date($0.at) ?? .distantPast) <= at }), last.id != rows.last?.id {
                p.byRow[last.id, default: []].append(it)
            } else {
                p.trailing.append(it)
            }
        }
        return p
    }
}

/// The content of one session (app.md 4.7): `content.list` merged with
/// `content.updated` events by id, bytes and diffs loaded on demand.
@MainActor
@Observable
public final class ContentModel {
    public let target: SessionTarget
    public private(set) var items: [ContentItem] = []
    public private(set) var context = ChatContext()
    /// The last list failed or the hab has no content capability; the hint says why.
    public private(set) var problem: String?
    /// The list was loaded at least once from the hab.
    public private(set) var loaded = false
    /// The session's folder on the hab, from the last list.
    public private(set) var folder: String?

    /// The largest file the hab serves (`hab-logic-api.md` 3.9).
    public static let maxFile = 32 << 20
    /// How long a burst of tool calls waits before the list loads again.
    @ObservationIgnored public var refreshDelay: Duration = .milliseconds(600)

    @ObservationIgnored private let connection: HabConnection?
    @ObservationIgnored private var listen: Task<Void, Never>?
    @ObservationIgnored private var pending: Task<Void, Never>?
    @ObservationIgnored private var viewers = 0
    /// Loaded bytes, bounded by total size, keyed by item and file version.
    @ObservationIgnored let cache = ByteCache(limit: ContentModel.cacheLimit)
    /// Loads in flight, by item and version: the inline card, the panel and a sheet share one.
    @ObservationIgnored private var inflight: [ByteCache.Key: Task<ContentBytes, Never>] = [:]
    /// Decoded images, by item, version and pixel size.
    @ObservationIgnored private var decoded: [String: CGImage] = [:]
    @ObservationIgnored private var decodedOrder: [String] = []
    /// Parsed Markdown, by item and version.
    @ObservationIgnored private var markdownCache: [ByteCache.Key: [MarkdownBlock]] = [:]
    /// Each list call's number; a reply older than the last one applied is dropped.
    @ObservationIgnored private var listGen = 0
    @ObservationIgnored private var appliedGen = 0
    /// Events seen while each list call runs, by its number.
    @ObservationIgnored private var racing: [Int: [ContentItem]] = [:]

    /// The byte cache's bound: about 64 MiB of file bytes.
    public static let cacheLimit = 64 << 20
    /// Decoded images kept at once.
    static let imageLimit = 24
    /// Tries of a content call the hab turned away as busy (`unavailable`).
    static let busyTries = 4
    /// The first wait after a busy answer; it doubles each try.
    @ObservationIgnored var busyBackoff: Duration = .milliseconds(200)

    public init(target: SessionTarget, connection: HabConnection?) {
        self.target = target
        self.connection = connection
    }

    /// A model over fixed items, for previews, demo mode and tests.
    public init(target: SessionTarget, items: [ContentItem], context: ChatContext) {
        self.target = target
        self.connection = nil
        self.context = context
        self.items = items
        self.loaded = true
    }

    // MARK: - Lifecycle

    public func start() {
        viewers += 1
        guard viewers == 1, let conn = connection else { return }
        listen = Task { [weak self] in
            let stream = await conn.contentUpdates()
            for await it in stream {
                guard let self else { return }
                self.receive(it)
            }
        }
        Task { await self.refresh() }
    }

    public func stop() {
        guard viewers > 0 else { return }
        viewers -= 1
        guard viewers == 0 else { return }
        listen?.cancel()
        listen = nil
        pending?.cancel()
        pending = nil
    }

    /// Takes the hab's state. A return to online lists again; the last list stays while offline.
    public func update(_ c: ChatContext) {
        let wasOnline = context.online
        context = c
        if viewers > 0, c.online, !wasOnline { scheduleRefresh() }
    }

    /// A chat message changed: a finished tool call may have touched a file.
    public func chatChanged(_ m: ChatMessage) {
        guard m.kind == .tool, let t = m.tool, t.status != .running else { return }
        scheduleRefresh()
    }

    /// The offline placeholder, or nil while the hab is online.
    public var offlineText: String? {
        guard !context.online else { return nil }
        let host = context.hostLabel.isEmpty ? "the hab" : context.hostLabel
        return "Not available while \(host) is offline"
    }

    // MARK: - Lists

    public var shown: [ContentItem] { items.filter { $0.shown == true } }
    public var documents: [ContentItem] { items.filter { $0.shown == true && $0.kind != .image } }
    public var images: [ContentItem] { items.filter { $0.kind == .image } }
    public var modified: [ContentItem] { items.filter { $0.change != nil } }

    /// Merges one item into a list by id: replaces it in place, else appends it.
    public nonisolated static func merge(_ list: [ContentItem], _ it: ContentItem) -> [ContentItem] {
        var out = list
        if let i = out.firstIndex(where: { $0.id == it.id }) {
            // A show of a path the session also changed keeps the change mark.
            var merged = it
            if merged.change == nil { merged.change = out[i].change }
            if merged.message == nil { merged.message = out[i].message }
            out.remove(at: i)
            out.append(merged)
        } else {
            out.append(it)
        }
        return out
    }

    /// A fresh list replaces what is held, keeping events newer than the list
    /// started (an event that raced the reply).
    public nonisolated static func replace(_ list: [ContentItem], keeping events: [ContentItem]) -> [ContentItem] {
        var out = list
        for e in events where !out.contains(e) { out = merge(out, e) }
        return out
    }

    func receive(_ it: ContentItem) {
        guard it.session == target.session else { return }
        items = Self.merge(items, it)
        for k in racing.keys { racing[k]?.append(it) }
        cache.keep(id: it.id, key: ByteCache.Key(it))
    }

    private func scheduleRefresh() {
        pending?.cancel()
        let delay = refreshDelay
        pending = Task { [weak self] in
            try? await Task.sleep(for: delay)
            guard !Task.isCancelled else { return }
            await self?.refresh()
        }
    }

    /// Loads the list from the hab. A reply that an answer to a later call already
    /// replaced is dropped.
    public func refresh() async {
        guard let conn = connection, let session = target.session, context.online else { return }
        listGen += 1
        let gen = listGen
        racing[gen] = []
        defer { racing[gen] = nil }
        do {
            let list = try await conn.call(HabMethods.contentList, ContentListParams(session: session))
            guard gen > appliedGen else { return }
            appliedGen = gen
            items = Self.replace(list.items, keeping: racing[gen] ?? [])
            folder = list.folder
            problem = nil
            loaded = true
            // Bytes of a file that changed since they were loaded go now, not when evicted.
            for it in items { cache.keep(id: it.id, key: ByteCache.Key(it)) }
        } catch let e as RPCError {
            problem = e.data?.hint ?? e.message
        } catch {
            // Offline mid-call: keep the last list; the placeholder says why.
        }
    }

    // MARK: - Bytes

    /// Loads an item's bytes in chunks. A file that changes between chunks is
    /// read again once; then what arrived shows with a mark. Two views asking for
    /// the same version share one load.
    public func bytes(for it: ContentItem) async -> ContentBytes {
        if let off = offlineText { return .offline(off) }
        if let s = it.size, s > Self.maxFile { return .tooLarge(s) }
        let key = ByteCache.Key(it)
        if let d = cache.get(id: it.id, key: key) { return .data(d, changedWhileLoading: false) }
        if let t = inflight[key] { return await t.value }
        let t = Task { await self.load(it, key: key) }
        inflight[key] = t
        let r = await t.value
        inflight[key] = nil
        return r
    }

    private func load(_ it: ContentItem, key: ByteCache.Key) async -> ContentBytes {
        guard let conn = connection, let session = target.session else { return .offline("Not available") }
        var last = Data()
        do {
            for _ in 0..<2 {
                var buf = Data()
                var first: ContentData?
                var changed = false
                while true {
                    let off = buf.count
                    let r = try await busyRetry { try await conn.callContent(HabMethods.contentRead, ContentReadParams(id: it.id, limit: 1 << 20, offset: off, session: session)) }
                    if let f = first, f.size != r.size || f.modified != r.modified {
                        changed = true
                        break
                    }
                    if first == nil { first = r }
                    buf.append(r.data)
                    if r.eof || r.data.isEmpty { break }
                }
                last = buf
                if !changed, first != nil {
                    // Kept under the version the list knows, so the next view finds it.
                    cache.put(id: it.id, key: key, data: buf)
                    return .data(buf, changedWhileLoading: false)
                }
            }
            return .data(last, changedWhileLoading: true)
        } catch let e as RPCError {
            switch e.kind {
            case .notFound?: return .gone
            case .invalid? where (it.size ?? 0) > Self.maxFile: return .tooLarge(it.size ?? 0)
            default: return .failed(Self.loadFailed)
            }
        } catch {
            if let off = offlineText { return .offline(off) }
            return .failed(Self.loadFailed)
        }
    }

    /// What a card says when the hab could not serve a file. The hab's own hint stays in
    /// the log: it is written for a developer, not for the card.
    public static let loadFailed = "Could not load this file"

    /// Runs a content call again, with a doubling wait, while the hab answers `unavailable`
    /// (busy) and the connection stays online.
    private func busyRetry<R>(_ call: () async throws -> R) async throws -> R {
        var wait = busyBackoff
        for attempt in 1... {
            do {
                return try await call()
            } catch let e as RPCError where e.kind == .unavailable && attempt < Self.busyTries && context.online {
                try? await Task.sleep(for: wait)
                wait *= 2
            }
        }
        fatalError("unreachable")
    }

    /// Loads an item's diff.
    public func diff(for it: ContentItem) async -> ContentDiffLoad {
        if let off = offlineText { return .offline(off) }
        guard let conn = connection, let session = target.session else { return .offline("Not available") }
        do {
            let d = try await busyRetry { try await conn.callContent(HabMethods.contentDiff, ContentDiffParams(id: it.id, session: session)) }
            return .diff(DiffParser.parse(d.diff), source: d.source.rawValue, truncated: d.truncated ?? false)
        } catch let e as RPCError where e.kind == .notFound {
            return .gone
        } catch is RPCError {
            return .failed(Self.loadFailed)
        } catch {
            if let off = offlineText { return .offline(off) }
            return .failed(Self.loadFailed)
        }
    }

    // MARK: - Decoded content

    /// An image decoded once, off the main actor, no larger than `maxPixel` on its long side.
    public func image(for it: ContentItem, data: Data, maxPixel: Int) async -> CGImage? {
        let k = "\(it.id)|\(it.size ?? -1)|\(it.modified ?? "")|\(maxPixel)"
        if let img = decoded[k] { return img }
        let img = await Task.detached(priority: .userInitiated) { ImageDecode.thumbnail(data, maxPixel: maxPixel) }.value
        guard let img else { return nil }
        decoded[k] = img
        decodedOrder.append(k)
        if decodedOrder.count > Self.imageLimit { decoded[decodedOrder.removeFirst()] = nil }
        return img
    }

    /// An item's Markdown, parsed once per version.
    public func markdown(for it: ContentItem, data: Data) -> [MarkdownBlock] {
        let key = ByteCache.Key(it)
        if let b = markdownCache[key] { return b }
        let blocks = MarkdownBlock.parse(ContentText.preview(data).text)
        if markdownCache.count > 32 { markdownCache.removeAll() }
        markdownCache[key] = blocks
        return blocks
    }

    nonisolated static func date(_ s: String) -> Date? {
        guard !s.isEmpty else { return nil }
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }
}

// MARK: - Diffs

/// A unified diff, split into files, hunks and lines.
public struct ParsedDiff: Equatable, Sendable {
    public struct File: Equatable, Sendable {
        public var oldPath: String?
        public var newPath: String?
        public var hunks: [Hunk] = []
        public var binary = false
        public var added: Int { hunks.reduce(0) { $0 + $1.lines.filter { $0.kind == .added }.count } }
        public var removed: Int { hunks.reduce(0) { $0 + $1.lines.filter { $0.kind == .removed }.count } }
    }

    public struct Hunk: Equatable, Sendable {
        public var header: String
        public var lines: [Line] = []
    }

    public struct Line: Equatable, Sendable {
        public enum Kind: Equatable, Sendable { case context, added, removed, note }
        public var kind: Kind
        public var text: String
        /// Line numbers in the old and new file, when the hunk header gives them.
        public var old: Int?
        public var new: Int?
    }

    public var files: [File] = []
}

public enum DiffParser {
    /// Parses unified diff text: `diff --git` and `---`/`+++` headers, `@@`
    /// hunks with or without line numbers, "\ No newline" notes, binary files.
    public static func parse(_ text: String) -> ParsedDiff {
        var out = ParsedDiff()
        var file: ParsedDiff.File?
        var hunk: ParsedDiff.Hunk?
        var oldNo: Int?
        var newNo: Int?
        func closeHunk() {
            if let h = hunk { file?.hunks.append(h) }
            hunk = nil
        }
        func closeFile() {
            closeHunk()
            if let f = file { out.files.append(f) }
            file = nil
        }
        func strip(_ p: String) -> String? {
            let p = p.split(separator: "\t", maxSplits: 1).first.map(String.init) ?? p
            if p == "/dev/null" { return nil }
            if p.hasPrefix("a/") || p.hasPrefix("b/") { return String(p.dropFirst(2)) }
            return p
        }
        var lines = text.components(separatedBy: "\n")
        if lines.last == "" { lines.removeLast() }
        var i = 0
        while i < lines.count {
            let l = lines[i]
            i += 1
            if l.hasPrefix("diff --git ") {
                closeFile()
                file = ParsedDiff.File()
                continue
            }
            // An empty line in a hunk is a context line whose space an editor dropped.
            let fileHeader = l.hasPrefix("--- ") && i < lines.count && lines[i].hasPrefix("+++ ")
            let bodyLine = l.isEmpty || l.hasPrefix(" ") || l.hasPrefix("+") || l.hasPrefix("-") || l.hasPrefix("\\")
            if hunk == nil || !bodyLine || fileHeader {
                if l.hasPrefix("--- "), i < lines.count, lines[i].hasPrefix("+++ ") {
                    // Headers start a new file unless a `diff --git` line just opened an empty one.
                    let opened = file.map { $0.hunks.isEmpty && hunk == nil && $0.oldPath == nil && $0.newPath == nil && !$0.binary } ?? false
                    if !opened {
                        closeFile()
                        file = ParsedDiff.File()
                    }
                    file?.oldPath = strip(String(l.dropFirst(4)))
                    file?.newPath = strip(String(lines[i].dropFirst(4)))
                    i += 1
                    continue
                }
                if l.hasPrefix("@@") {
                    if file == nil { file = ParsedDiff.File() }
                    closeHunk()
                    hunk = ParsedDiff.Hunk(header: l)
                    (oldNo, newNo) = numbers(l)
                    continue
                }
                if l.hasPrefix("Binary files ") || l == "Binary file" || l.hasPrefix("GIT binary patch") {
                    if file == nil { file = ParsedDiff.File() }
                    file?.binary = true
                }
                // index, mode and rename lines carry nothing the view shows.
                continue
            }
            switch l.first {
            case "+":
                hunk?.lines.append(.init(kind: .added, text: String(l.dropFirst()), old: nil, new: newNo))
                newNo = newNo.map { $0 + 1 }
            case "-":
                hunk?.lines.append(.init(kind: .removed, text: String(l.dropFirst()), old: oldNo, new: nil))
                oldNo = oldNo.map { $0 + 1 }
            case "\\":
                hunk?.lines.append(.init(kind: .note, text: String(l.dropFirst(2)), old: nil, new: nil))
            default:
                hunk?.lines.append(.init(kind: .context, text: String(l.dropFirst()), old: oldNo, new: newNo))
                oldNo = oldNo.map { $0 + 1 }
                newNo = newNo.map { $0 + 1 }
            }
        }
        closeFile()
        return out
    }

    /// The start lines of a `@@ -a,b +c,d @@` header; nil for a header without numbers.
    static func numbers(_ header: String) -> (Int?, Int?) {
        let parts = header.split(separator: " ")
        guard parts.count >= 3, parts[1].hasPrefix("-"), parts[2].hasPrefix("+") else { return (nil, nil) }
        func start(_ s: Substring) -> Int? { Int(s.dropFirst().split(separator: ",").first ?? "") }
        return (start(parts[1]), start(parts[2]))
    }
}
