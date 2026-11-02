import Foundation
import HabAPI

/// The Capcom composer's context chips (capcom.md 8.1, 8.4; app.md 4.9): what waits for the
/// next message of a Capcom session, as the hab lists it in each `capcom.open`,
/// `capcom.attach` and `capcom.detach` result. The hab resolves every ref and delivers the
/// items before the text at the next `session.send`, then drops them; the model drops the
/// same items once that send succeeds and keeps them on any error.
extension ChatModel {
    /// Takes the list a `capcom.open` result gave for this session. Absent means none.
    public func takePending(_ items: [CapcomContextItem]?) {
        pending = items ?? []
        let refs = Set(pending.filter { $0.kind == .file }.map(\.ref))
        chipFiles = chipFiles.filter { refs.contains($0.key) }
    }

    /// Attaches one item to the next message. Returns false when the hab refused it; the
    /// hab's text then shows under the composer.
    @discardableResult
    public func attach(_ ref: CapcomContextRef) async -> Bool {
        guard let conn = connection, let session = target.session else { return false }
        capcomBusy = true
        defer { capcomBusy = false }
        do {
            let r = try await conn.call(HabMethods.capcomAttach, CapcomAttachParams(host: ref.host, kind: ref.kind, ref: ref.ref, session: session))
            notice = nil
            takePending(r.pending)
            return true
        } catch {
            notice = Self.describe(error)
            return false
        }
    }

    /// Attaches a console command by its pane and command id.
    @discardableResult
    public func attachCommand(pane: String, command: UInt64) async -> Bool {
        await attach(CapcomContextRef(kind: .command, ref: "\(pane):\(command)"))
    }

    /// Uploads a file into the Capcom session's folder with `attachment.put`, then attaches
    /// it as a chip. A file over the limit is refused before any upload.
    @discardableResult
    public func attachFile(_ a: DraftAttachment) async -> Bool {
        guard let conn = connection, let session = target.session else { return false }
        if let r = AttachmentLimits.refusal(a) {
            notice = r
            return false
        }
        guard await conn.has(capability: AttachmentLimits.capability) else {
            notice = "This hab cannot take files; update gff-hab."
            return false
        }
        capcomBusy = true
        let up: UploadedAttachment
        do {
            up = try await AttachmentUpload.run(a, session: session, connection: conn)
        } catch {
            capcomBusy = false
            notice = Self.describe(error)
            return false
        }
        capcomBusy = false
        chipFiles[up.id] = a
        let ok = await attach(CapcomContextRef(kind: .file, ref: up.id))
        if !ok { chipFiles[up.id] = nil }
        return ok
    }

    /// Removes one chip with `capcom.detach`. An item that no longer waits (`not_found`)
    /// leaves the chips too: the hab no longer holds it.
    public func detach(_ item: CapcomContextItem) async {
        guard let conn = connection, let session = target.session else { return }
        capcomBusy = true
        defer { capcomBusy = false }
        do {
            let r = try await conn.call(HabMethods.capcomDetach, CapcomDetachParams(host: detachHost(item), kind: item.kind, ref: item.ref, session: session))
            notice = nil
            takePending(r.pending)
        } catch let e as RPCError where e.kind == .notFound {
            dropChips([item])
        } catch {
            notice = Self.describe(error)
        }
    }

    /// Reads what waits with `capcom.pending`, which starts and resumes nothing, so any Capcom
    /// session, running or exited, standing or older, reads its own chips. A hab from before
    /// that call answers kind `invalid` (method not found). Any error leaves the chips as the
    /// app last saw them.
    public func refreshPending() async {
        guard let conn = connection, let session = target.session, context.isCapcom, context.online,
              await conn.has(capability: CapcomChip.capability) else { return }
        guard let r = try? await conn.call(HabMethods.capcomPending, CapcomPendingParams(session: session)), r.session == session else { return }
        takePending(r.pending)
    }

    /// Drops chips the hab no longer holds.
    func dropChips(_ items: [CapcomContextItem]) {
        pending.removeAll { items.contains($0) }
        for it in items where it.kind == .file { chipFiles[it.ref] = nil }
    }

    /// The host a detach names: none for this hab, else the item's label.
    func detachHost(_ item: CapcomContextItem) -> String? {
        CapcomChip.isLocal(item, hostID: context.hostID) ? nil : item.host
    }

    /// The ref that attaches the same item again elsewhere on this hab. A file's ref names an
    /// attachment of this session, so it has none.
    func reattachRef(_ item: CapcomContextItem) -> CapcomContextRef? {
        guard item.kind != .file else { return nil }
        return CapcomContextRef(host: item.kind == .notification ? detachHost(item) : nil, kind: item.kind, ref: item.ref)
    }
}

/// How a chip looks: its symbol, and the host shown beside its title.
public enum CapcomChip {
    public static let capability = "capcom"

    /// Whether the item came from the hab it waits on.
    public static func isLocal(_ item: CapcomContextItem, hostID: String) -> Bool {
        item.host.isEmpty || item.host == hostID || item.host == "self"
    }

    /// The chip's text: the title, with the host label when it is another hab.
    public static func label(_ item: CapcomContextItem, hostID: String) -> String {
        isLocal(item, hostID: hostID) ? item.title : "\(item.title) · \(item.host)"
    }

    public static func symbol(_ kind: CapcomContextKind) -> String {
        switch kind {
        case .notification: return "bell"
        case .session: return "bubble.left.and.bubble.right"
        case .file: return "doc"
        case .command: return "apple.terminal"
        case .logbook: return "book.closed"
        default: return "paperclip"
        }
    }
}

/// One context block a Capcom user message carried, as the hab wrote it (capcom.md 8.4).
public struct CapcomContextBlock: Hashable, Sendable {
    public var kind: String
    public var ref: String
    public var host: String
    /// The block's first field: the notice title, the session name, the file path or the
    /// command line.
    public var title: String
}

/// Reads the context blocks the hab puts before a Capcom message's text (capcom.md 8.4): an
/// opening `[Flightdeck: context ...]` line, then `[begin context <kind> <ref> on <host>]` ...
/// `[end context <kind> <ref>]` per item, then the user's text. Resolved lines never start at
/// column 0 with `[` (the hab indents and cleans them), so only the hab's markers match.
public enum CapcomContextText {
    static let opening = "[Flightdeck: context "

    /// The blocks and the user's text after the last one (empty for context alone), or nil
    /// when the text does not start with the opening line.
    public static func split(_ text: String) -> (blocks: [CapcomContextBlock], text: String)? {
        guard text.hasPrefix(opening) else { return nil }
        let lines = text.components(separatedBy: "\n")
        var i = 1
        var blocks: [CapcomContextBlock] = []
        while i < lines.count, let head = begin(lines[i]) {
            var title = ""
            var j = i + 1
            let end = "[end context \(head.kind) \(head.ref)]"
            while j < lines.count, lines[j] != end {
                let l = lines[j]
                if title.isEmpty, !l.hasPrefix(" "), let colon = l.range(of: ": ") {
                    title = String(l[colon.upperBound...])
                }
                j += 1
            }
            // A block with no end line is not one the hab wrote.
            guard j < lines.count else { return nil }
            blocks.append(CapcomContextBlock(kind: head.kind, ref: head.ref, host: head.host, title: title))
            i = j + 1
        }
        guard !blocks.isEmpty else { return nil }
        return (blocks, i < lines.count ? lines[i...].joined(separator: "\n") : "")
    }

    /// The kind, ref and host of a begin line.
    private static func begin(_ line: String) -> (kind: String, ref: String, host: String)? {
        let pre = "[begin context ", post = "]"
        guard line.hasPrefix(pre), line.hasSuffix(post) else { return nil }
        let inner = line.dropFirst(pre.count).dropLast(post.count)
        guard let on = inner.range(of: " on ", options: .backwards) else { return nil }
        let head = inner[..<on.lowerBound]
        guard let sp = head.firstIndex(of: " ") else { return nil }
        return (String(head[..<sp]), String(head[head.index(after: sp)...]), String(inner[on.upperBound...]))
    }

    /// The user's own text of a transcript record: after the blocks, or all of it.
    public static func userText(_ text: String) -> String {
        split(text)?.text ?? text
    }
}
