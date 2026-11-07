import Foundation
import HabAPI

/// One earlier thread: a host that was the Capcom home keeps its thread, read-only, after a
/// re-assignment (capcom-service.md 7.1, 14).
public struct CapcomEarlierThread: Identifiable {
    public var id: String { habID }
    public let habID: String
    public let label: String
    public let thread: CapcomThread
}

extension CapcomCenter {
    /// Reads the thread of every other online hab that runs the Capcom service, through its
    /// `capcom.thread.page`, and keeps those that hold any items. The home's own thread is
    /// the one the view shows; it never lists here.
    public func loadEarlier() async {
        var home: String?
        if case .ready(let id) = entry { home = id }
        if case .offline(let id) = entry { home = id }
        var out: [CapcomEarlierThread] = []
        for h in habs.statuses() where h.status == .online && h.id != home {
            guard let conn = habs.connection(h.id), await conn.has(capability: CapcomThread.capability) else { continue }
            let t = earlier.first(where: { $0.habID == h.id })?.thread ?? CapcomThread(habID: h.id, session: nil, connection: conn)
            await t.loadTail()
            if !t.items.isEmpty { out.append(CapcomEarlierThread(habID: h.id, label: habs.label(h.id), thread: t)) }
        }
        earlier = out
    }

    /// Hands a reply that did not reach the hab to the standing conversation's composer.
    func takeUnsent(into m: ChatModel) {
        guard let text = unsent, !text.isEmpty else { return }
        unsent = nil
        m.handoff = m.handoff.map { $0 + "\n" + text } ?? text
    }
}
