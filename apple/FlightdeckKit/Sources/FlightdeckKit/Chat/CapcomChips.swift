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

    /// Reads what waits from `capcom.open` with no context, when the session runs and the hab
    /// answers for it as the standing conversation. Other answers change nothing.
    public func refreshPending() async {
        guard let conn = connection, let session = target.session, context.isCapcom, context.online,
              !context.sessionExited, await conn.has(capability: CapcomChip.capability) else { return }
        guard let r = try? await conn.call(HabMethods.capcomOpen, CapcomOpenParams()), r.session == session else { return }
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
