import CoreTransferable
import Foundation
import UniformTypeIdentifiers

/// A file read for the composer, or why it was not (app.md 4.4).
public enum AttachmentLoad: Equatable, Sendable {
    case file(DraftAttachment)
    case refused(String)
}

extension AttachmentPrep {
    /// Reads a file the owner picked in Files or dropped on a chat: the one loader for both
    /// (app.md 4.4). A folder, a file over the limit or one that cannot be read is refused with
    /// a sentence; an image is prepared as a picked photo is.
    public static func load(_ url: URL) -> AttachmentLoad {
        let name = url.lastPathComponent.isEmpty ? "file" : url.lastPathComponent
        let scoped = url.startAccessingSecurityScopedResource()
        defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        let values = try? url.resourceValues(forKeys: [.isDirectoryKey, .fileSizeKey])
        if values?.isDirectory == true { return .refused("\(name) is a folder; attach the files in it") }
        if let size = values?.fileSize, size > AttachmentLimits.maxBytes { return .refused("\(name) is over the 25 MB limit") }
        guard let data = try? Data(contentsOf: url) else { return .refused("Couldn't read \(name)") }
        let a = file(data, name: name)
        if let r = AttachmentLimits.refusal(a) { return .refused(r) }
        return .file(a)
    }

    /// `load` off the caller's actor: reading and preparing 25 MB must not hold the main actor.
    @concurrent public static func loading(_ url: URL) async -> AttachmentLoad {
        load(url)
    }
}

/// One item dropped on a chat (app.md 4.4). The system takes the first representation the
/// dragged item offers, in this order: a URL (the Mac's Finder gives file URLs; a web link is
/// text), an image (Photos), text, then any other file (the iPad's Files).
public enum ChatDropItem: Transferable, Equatable, Sendable {
    /// A file by its URL, read when the drop lands. The owner's file: never deleted.
    case file(URL)
    /// The app's own copy of a file the system handed over, read when the drop lands within
    /// the file cap, then deleted.
    case copy(URL)
    /// A file refused when it arrived, before any copy.
    case refused(String)
    /// Text or a web link: it goes into the draft.
    case text(String)

    /// A dragged URL: a file URL is a file; any other link is text.
    public static func of(_ url: URL) -> ChatDropItem {
        url.isFileURL ? .file(url) : .text(url.absoluteString)
    }

    /// A file the system handed over: the system deletes it once the representation's closure
    /// returns, so it is copied to a folder of its own, keeping its name. Reading waits for
    /// `ChatDrop.prepare`, so a file past the cap is never read into memory. A folder or a
    /// file over the limit is refused here, before the copy.
    public static func copying(_ received: URL) -> ChatDropItem {
        let name = received.lastPathComponent.isEmpty ? "file" : received.lastPathComponent
        let values = try? received.resourceValues(forKeys: [.isDirectoryKey, .fileSizeKey])
        if values?.isDirectory == true { return .refused("\(name) is a folder; attach the files in it") }
        if let size = values?.fileSize, size > AttachmentLimits.maxBytes { return .refused("\(name) is over the 25 MB limit") }
        let dir = ChatDrop.copies.appendingPathComponent(UUID().uuidString, isDirectory: true)
        let dst = dir.appendingPathComponent(name)
        do {
            try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
            try FileManager.default.copyItem(at: received, to: dst)
        } catch {
            try? FileManager.default.removeItem(at: dir)
            return .refused("Couldn't read \(name)")
        }
        return .copy(dst)
    }

    public static var transferRepresentation: some TransferRepresentation {
        ProxyRepresentation(importing: { (url: URL) in ChatDropItem.of(url) })
        FileRepresentation(importedContentType: .image) { received in ChatDropItem.copying(received.file) }
        ProxyRepresentation(importing: { (text: String) in ChatDropItem.text(text) })
        FileRepresentation(importedContentType: .item) { received in ChatDropItem.copying(received.file) }
    }
}

/// Dropping files on a chat (app.md 4.4): what the drop adds to the composer.
public enum ChatDrop {
    /// The most files a message holds, as the Photos picker allows: a drop counts the files
    /// the composer holds already.
    public static let maxFiles = 10
    /// The most dropped text that goes into the draft, in UTF-8 bytes. A text file dragged from
    /// Files can arrive as text; past this it is refused instead of filling the composer.
    public static let maxTextBytes = 100_000

    /// Where the copies of handed-over files wait for `prepare`.
    static let copies = FileManager.default.temporaryDirectory.appendingPathComponent("chat-drop", isDirectory: true)

    /// What a drop gave the composer.
    public struct Outcome: Equatable, Sendable {
        public var attachments: [DraftAttachment] = []
        /// Text and links, one per line, for the draft; nil when the drop held none.
        public var text: String?
        /// Why files were not attached, one sentence each.
        public var notices: [String] = []

        /// The line under the composer: the first refusal, and how many more there were.
        public var notice: String? {
            guard let first = notices.first else { return nil }
            let more = notices.count - 1
            return more == 0 ? first : "\(first) (\(more) more not attached)"
        }
    }

    /// The composer takes files while its attach button does: not while it is off or answers
    /// a question.
    public static func takesFiles(_ mode: ChatAnswer.ComposerMode) -> Bool {
        mode != .off && mode != .answer
    }

    static func capNotice(_ name: String) -> String {
        "\(name) was not attached: at most \(maxFiles) files in a message"
    }

    /// Reads the dropped items in order, off the caller's actor, while the composer holds
    /// fewer than `maxFiles` with the `holding` it has. A file past the cap is not read. Every
    /// copy is deleted, read or not.
    @concurrent public static func prepare(_ items: [ChatDropItem], holding: Int = 0) async -> Outcome {
        defer { discard(items) }
        var out = Outcome()
        var texts: [String] = []
        var textBytes = 0
        var files = holding
        for item in items {
            let load: AttachmentLoad
            switch item {
            case .text(let t):
                let s = t.trimmingCharacters(in: .whitespacesAndNewlines)
                guard !s.isEmpty else { continue }
                let n = s.utf8.count
                if textBytes + n > maxTextBytes {
                    out.notices.append("The dropped text is over 100 KB; attach it as a file")
                    continue
                }
                textBytes += n
                texts.append(s)
                continue
            case .file(let url), .copy(let url):
                if files >= maxFiles {
                    out.notices.append(capNotice(url.lastPathComponent))
                    continue
                }
                load = AttachmentPrep.load(url)
            case .refused(let why):
                load = .refused(why)
            }
            switch load {
            case .file(let a):
                out.attachments.append(a)
                files += 1
            case .refused(let why):
                out.notices.append(why)
            }
        }
        out.text = texts.isEmpty ? nil : texts.joined(separator: "\n")
        return out
    }

    /// Deletes the copies among `items`, for a drop that is refused or done.
    public static func discard(_ items: [ChatDropItem]) {
        for case .copy(let url) in items {
            // A copy sits alone in a folder of its own under `copies`: remove the folder.
            let dir = url.deletingLastPathComponent()
            let own = dir.deletingLastPathComponent().standardizedFileURL.path == copies.standardizedFileURL.path
            try? FileManager.default.removeItem(at: own ? dir : url)
        }
    }

    /// A drop's outcome landing in the composer, checked again now that it is read: nothing
    /// lands when the composer stopped taking files meanwhile, and the files stay within
    /// `maxFiles` with what the composer holds now. Returns the notice for the composer.
    public static func land(_ out: Outcome, takesFiles: Bool, draft: inout String,
                            attachments: inout [DraftAttachment]) -> String? {
        guard takesFiles else {
            return out.attachments.isEmpty && out.text == nil ? out.notice : "The drop was not added: the composer takes no files now"
        }
        var notices = out.notices
        let room = max(0, maxFiles - attachments.count)
        attachments += out.attachments.prefix(room)
        notices += out.attachments.dropFirst(room).map { capNotice($0.name) }
        draft = self.draft(draft, adding: out.text)
        return Outcome(notices: notices).notice
    }

    /// The draft with dropped text added on a line of its own.
    public static func draft(_ draft: String, adding text: String?) -> String {
        guard let text, !text.isEmpty else { return draft }
        return draft.isEmpty ? text : draft + "\n" + text
    }
}
