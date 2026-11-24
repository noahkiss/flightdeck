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
public enum ChatDropItem: Transferable, Sendable {
    /// A file by its URL, read when the drop lands.
    case file(URL)
    /// A file the system handed over as a copy, read while the copy existed.
    case loaded(AttachmentLoad)
    /// Text or a web link: it goes into the draft.
    case text(String)

    /// A dragged URL: a file URL is a file; any other link is text.
    public static func of(_ url: URL) -> ChatDropItem {
        url.isFileURL ? .file(url) : .text(url.absoluteString)
    }

    public static var transferRepresentation: some TransferRepresentation {
        ProxyRepresentation(importing: { (url: URL) in ChatDropItem.of(url) })
        // The system deletes its copy once the closure returns: read it here.
        FileRepresentation(importedContentType: .image) { received in ChatDropItem.loaded(AttachmentPrep.load(received.file)) }
        ProxyRepresentation(importing: { (text: String) in ChatDropItem.text(text) })
        FileRepresentation(importedContentType: .item) { received in ChatDropItem.loaded(AttachmentPrep.load(received.file)) }
    }
}

/// Dropping files on a chat (app.md 4.4): what the drop adds to the composer.
public enum ChatDrop {
    /// The most files one drop attaches, as the Photos picker allows.
    public static let maxFiles = 10

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

    /// Reads the dropped items in order, off the caller's actor.
    @concurrent public static func prepare(_ items: [ChatDropItem]) async -> Outcome {
        var out = Outcome()
        var texts: [String] = []
        var files = 0
        for item in items {
            let load: AttachmentLoad
            switch item {
            case .text(let t):
                let s = t.trimmingCharacters(in: .whitespacesAndNewlines)
                if !s.isEmpty { texts.append(s) }
                continue
            case .file(let url):
                if files >= maxFiles {
                    out.notices.append("\(url.lastPathComponent) was not attached: at most \(maxFiles) files at once")
                    continue
                }
                load = AttachmentPrep.load(url)
            case .loaded(let l):
                load = l
            }
            switch load {
            case .file(let a):
                if files >= maxFiles {
                    out.notices.append("\(a.name) was not attached: at most \(maxFiles) files at once")
                } else {
                    out.attachments.append(a)
                    files += 1
                }
            case .refused(let why):
                out.notices.append(why)
            }
        }
        out.text = texts.isEmpty ? nil : texts.joined(separator: "\n")
        return out
    }

    /// The draft with dropped text added on a line of its own.
    public static func draft(_ draft: String, adding text: String?) -> String {
        guard let text, !text.isEmpty else { return draft }
        return draft.isEmpty ? text : draft + "\n" + text
    }
}
