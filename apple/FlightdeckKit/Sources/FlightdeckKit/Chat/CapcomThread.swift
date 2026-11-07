import Foundation
import HabAPI
import Observation

/// The Capcom service's thread (capcom-service.md 7.2 to 7.6), as the app shows it: the items
/// the home hab keeps, paged with `capcom.thread.page` and kept current by `capcom.item` and
/// `capcom.typing`, the typing mark, Stop, card answers, and the pinned proposals line.
/// One per service session; the session's chat model owns it.
@MainActor
@Observable
public final class CapcomThread {
    public let habID: String
    /// The service session, for the typing mark; nil for an earlier thread read from another
    /// host, which takes no answers.
    public let session: String?
    /// The items, oldest first, folded by id.
    public private(set) var items: [CapcomItem] = []
    public private(set) var load: ChatLoad = .loading
    public private(set) var hasOlder = false
    public private(set) var loadingOlder = false
    /// Capcom is typing: a user turn runs (7.3).
    public private(set) var typing = false
    /// The last Stop or answer error, shown under the thread.
    public internal(set) var notice: String?
    /// Card answers on their way, by item id.
    public private(set) var answering: Set<String> = []
    /// Pending proposals the Capcom session filed (6.4), for the pinned line. Nil: unknown.
    public private(set) var proposals: [LogbookRecord]?

    /// Page size of `capcom.thread.page`; the hab's ceiling is 200.
    @ObservationIgnored public var pageSize = 100
    @ObservationIgnored let connection: HabConnection?
    @ObservationIgnored private var index: [String: Int] = [:]
    @ObservationIgnored private var listen: Task<Void, Never>?
    @ObservationIgnored private var viewers = 0
    @ObservationIgnored private var generation: Int?
    @ObservationIgnored private var proposalsBusy = false
    @ObservationIgnored private var proposalsAgain = false
    /// Items that arrive while a page loads; applied after it.
    @ObservationIgnored private var buffer: [CapcomItem]?
    /// One newest-page read at a time; a call meanwhile reads once more after it.
    @ObservationIgnored private var tailBusy = false
    @ObservationIgnored private var tailAgain = false

    public init(habID: String, session: String?, connection: HabConnection?) {
        self.habID = habID
        self.session = session
        self.connection = connection
    }

    /// A thread over fixed items, for previews and snapshots.
    public init(habID: String, session: String?, items: [CapcomItem], typing: Bool = false) {
        self.habID = habID
        self.session = session
        self.connection = nil
        self.typing = typing
        self.load = .ready
        replaceAll(items)
    }

    // MARK: - Lifecycle

    /// Starts listening and loads the newest page. Each `start` needs its own `stop`.
    public func start() {
        viewers += 1
        guard viewers == 1, let conn = connection else { return }
        listen = Task { [weak self] in
            let stream = await conn.capcomUpdates()
            for await u in stream {
                guard let self else { return }
                self.receive(u)
            }
        }
        Task { await self.loadTail() }
    }

    public func stop() {
        guard viewers > 0 else { return }
        viewers -= 1
        guard viewers == 0 else { return }
        listen?.cancel()
        listen = nil
    }

    /// The hab came back online: read the newest page again, which folds every change missed.
    public func reconnected() {
        guard viewers > 0 else { return }
        Task { await self.loadTail() }
    }

    // MARK: - Pages

    /// Loads the newest page and merges it with what is held. One read runs at a time, so an
    /// older page never lands over items an event already changed. The typing mark has no
    /// snapshot (7.4): it is read again from `capcom.status`, so a missed "off" does not stick.
    public func loadTail() async {
        guard let conn = connection else { return }
        if tailBusy {
            tailAgain = true
            return
        }
        tailBusy = true
        defer { tailBusy = false }
        repeat {
            tailAgain = false
            if items.isEmpty { load = .loading }
            buffer = buffer ?? []
            do {
                let page = try await conn.call(HabMethods.capcomThreadPage, CapcomThreadPageParams(limit: pageSize))
                let fresh = items.isEmpty
                for it in page.items.reversed() { apply(it) }
                if fresh { hasOlder = page.more }
                load = .ready
            } catch {
                if items.isEmpty { load = ChatModel.loadState(for: error) }
            }
            flush()
            if session != nil {
                if let st = try? await conn.call(HabMethods.capcomStatus, CapcomServiceParams()) {
                    typing = typing && st.turn != nil
                }
            }
        } while tailAgain
        if session != nil { await refreshProposals() }
    }

    /// The hab went offline: no typing can be known until it is back.
    public func wentOffline() { typing = false }

    /// Loads the page before the oldest item held.
    public func loadOlder() async {
        guard let conn = connection, hasOlder, !loadingOlder, let oldest = items.first?.id else { return }
        loadingOlder = true
        defer { loadingOlder = false }
        do {
            let page = try await conn.call(HabMethods.capcomThreadPage, CapcomThreadPageParams(before: oldest, limit: pageSize))
            for it in page.items { apply(it) }
            hasOlder = page.more
        } catch {
            notice = "Could not load earlier messages: \(ChatModel.describe(error))"
        }
    }

    private func flush() {
        let held = buffer ?? []
        buffer = nil
        for it in held { apply(it) }
    }

    private func receive(_ u: CapcomUpdate) {
        switch u {
        case .item(let it, let g):
            if let generation, g != generation {
                // A new connection: events may have been missed while it changed hands.
                self.generation = g
                Task { await self.loadTail() }
            }
            if generation == nil { generation = g }
            if buffer != nil { buffer?.append(it) } else { apply(it) }
            // A proposal Capcom filed shows as a card; the pinned line counts again.
            if it.kind.rawValue == "card" || it.kind.rawValue == "note" { Task { await self.refreshProposals() } }
        case .typing(let s, let on, _):
            guard s == session else { return }
            typing = on
        }
    }

    /// Adds or replaces one item, keeping the list in id order.
    func apply(_ it: CapcomItem) {
        if let i = index[it.id] {
            items[i] = it
            return
        }
        if let last = items.last, last.id > it.id {
            items.append(it)
            items.sort { $0.id < $1.id }
            rebuildIndex()
        } else {
            index[it.id] = items.count
            items.append(it)
        }
    }

    private func replaceAll(_ its: [CapcomItem]) {
        items = its.sorted { $0.id < $1.id }
        rebuildIndex()
    }

    private func rebuildIndex() {
        index = [:]
        for (i, it) in items.enumerated() { index[it.id] = i }
    }

    // MARK: - Actions

    /// Stops the current turn (`capcom.interrupt`, 7.4).
    public func stopTurn() async {
        guard let conn = connection else { return }
        do {
            _ = try await conn.call(HabMethods.capcomInterrupt, CapcomServiceParams())
            notice = nil
        } catch {
            notice = "Stop failed: \(ChatModel.refusal(error))"
        }
    }

    /// Answers an approval card: allow, allow_site or deny (5.3). The first answer wins; a
    /// later one shows the hab's reason.
    public func answer(_ item: CapcomItem, _ choice: ApprovalChoice) async {
        guard let conn = connection, session != nil, let card = item.card, card.type == "approval", let ref = card.ref,
              !answering.contains(item.id) else { return }
        answering.insert(item.id)
        defer { answering.remove(item.id) }
        do {
            _ = try await conn.call(HabMethods.approvalAnswer, ApprovalAnswerParams(choice: choice, pane: CapcomThread.engineRoute, request: ref, via: .app))
            notice = nil
        } catch {
            notice = ChatModel.refusal(error)
        }
    }

    /// The hab runs the Capcom service: the thread calls and events.
    public nonisolated static let capability = "capcom-thread"

    /// The engine route's name: approval answers for the service session name it as their pane.
    public nonisolated static let engineRoute = "capcom-engine"

    /// Reads the pending proposals the Capcom session filed (6.4). A hab without Logbook, or a
    /// failed read, leaves the line as it was.
    public func refreshProposals() async {
        guard let conn = connection, let session else { return }
        if proposalsBusy {
            proposalsAgain = true
            return
        }
        proposalsBusy = true
        defer { proposalsBusy = false }
        repeat {
            proposalsAgain = false
            guard await conn.has(capability: "logbook") else { return }
            if let r = try? await conn.call(HabMethods.logbookList, LogbookListParams(session: session, status: "pending", type: "proposal")) {
                proposals = r.records
            }
        } while proposalsAgain
    }

    /// The pinned line's words (6.4): "3 proposals to review"; nil with none.
    public nonisolated static func proposalsLine(_ n: Int) -> String? {
        switch n {
        case ..<1: return nil
        case 1: return "1 proposal to review"
        default: return "\(n) proposals to review"
        }
    }

    // MARK: - Reading

    /// The rows the thread view draws, oldest first.
    public var rows: [CapcomRow] { items.map(CapcomRow.of) }

    /// The approval cards still open, by item id.
    public func isOpen(_ it: CapcomItem) -> Bool { it.card != nil && it.answer == nil }
}

/// A choice on a Capcom approval card (5.3), in the order the card shows them: Deny first,
/// Allow last, as the approval sheet orders them (app.md 7.2).
public enum CapcomCardChoice: String, CaseIterable, Sendable {
    case deny
    case allowSite = "allow_site"
    case allow

    public var title: String {
        switch self {
        case .deny: return "Deny"
        case .allowSite: return "Allow this site"
        case .allow: return "Allow"
        }
    }

    public var answer: ApprovalChoice {
        switch self {
        case .deny: return .deny
        case .allowSite: return .allowSite
        case .allow: return .allow
        }
    }

    /// The buttons a card shows: the choices the hab offers, in this order.
    public static func buttons(_ it: CapcomItem) -> [CapcomCardChoice] {
        let offered = Set(it.card?.choices ?? [])
        return allCases.filter { offered.contains($0.rawValue) }
    }

    /// The answered line: "Allowed", "Allowed this site", "Denied", "Expired".
    public static func answered(_ a: CapcomAnswer) -> String {
        switch a.choice {
        case "allow": return "Allowed"
        case "allow_site": return "Allowed this site"
        case "deny": return "Denied"
        case "expired": return "Expired"
        default: return a.choice.capitalized
        }
    }
}

/// One row of the Capcom thread (capcom-service.md 7.2), in the words it shows.
public enum CapcomRow: Equatable, Identifiable, Sendable {
    /// The user's message: its text, thumbnails, context chips, and read (true) or delivered.
    case user(id: String, text: String, attachments: [String], context: [CapcomContextItem], read: Bool)
    /// Capcom's message; `wroteFirst` when an event turn wrote it.
    case capcom(id: String, text: String, wroteFirst: Bool)
    /// A turn's tool calls, folded into one quiet line: its summary, then each call.
    case tools(id: String, line: String, calls: [CapcomToolCall])
    /// A quiet line that opens to a text: an agent's report, a round message.
    case quiet(id: String, line: String, detail: String)
    /// A card: approval, confirmation, hand-off, calendar, reminder or proposal.
    case card(CapcomItem)
    /// A one-line Flightdeck note.
    case note(id: String, text: String)
    /// A divider with its label, and the summary the fresh SDK session opened with.
    case divider(id: String, label: String, summary: String)

    public var id: String {
        switch self {
        case .user(let id, _, _, _, _), .capcom(let id, _, _), .tools(let id, _, _), .quiet(let id, _, _),
             .note(let id, _), .divider(let id, _, _): return id
        case .card(let it): return it.id
        }
    }

    public static func of(_ it: CapcomItem) -> CapcomRow {
        let text = it.text ?? ""
        switch it.kind.rawValue {
        case "message":
            if it.author.kind.rawValue == "user" {
                return .user(id: it.id, text: text, attachments: it.attachments ?? [], context: it.context ?? [], read: it.read != nil)
            }
            return .capcom(id: it.id, text: text, wroteFirst: it.event == true)
        case "tools":
            return .tools(id: it.id, line: toolsLine(it.tools ?? []), calls: it.tools ?? [])
        case "agent":
            let from = it.author.address.map { "Report from \($0)" } ?? "Report from an agent"
            return .quiet(id: it.id, line: from, detail: text)
        case "round":
            return .quiet(id: it.id, line: firstLine(text).isEmpty ? "Round message" : firstLine(text), detail: text)
        case "card":
            return .card(it)
        case "divider":
            return .divider(id: it.id, label: text.isEmpty ? "Earlier messages summarised" : text, summary: it.summary ?? "")
        case "note":
            return .note(id: it.id, text: text)
        default:
            // An unknown kind shows as a quiet line (api: CapcomItemKind is open).
            return .note(id: it.id, text: text.isEmpty ? it.kind.rawValue : text)
        }
    }

    /// "read 3 transcripts, searched the web": each tool once, with its count, in order.
    public static func toolsLine(_ calls: [CapcomToolCall]) -> String {
        var order: [String] = []
        var count: [String: Int] = [:]
        for c in calls {
            let w = toolWords(c.name)
            if count[w] == nil { order.append(w) }
            count[w, default: 0] += 1
        }
        let parts = order.map { w -> String in
            let n = count[w] ?? 1
            return n > 1 ? "\(w) ×\(n)" : w
        }
        return parts.isEmpty ? "Used no tools" : parts.joined(separator: ", ")
    }

    static func toolWords(_ name: String) -> String {
        var n = name
        for p in ["mcp__flightdeck__", "flightdeck_"] where n.hasPrefix(p) { n = String(n.dropFirst(p.count)) }
        switch n {
        case "WebSearch": return "searched the web"
        case "WebFetch": return "opened a page"
        case "transcript_search": return "searched transcripts"
        case "transcript_read": return "read a transcript"
        case "logbook_search", "logbook_read", "logbook_brief": return "read Logbook"
        case "logbook_write", "file_record": return "wrote to Logbook"
        case "hand_off": return "handed off"
        case "start_session": return "started a session"
        default: return n.replacingOccurrences(of: "_", with: " ")
        }
    }

    static func firstLine(_ s: String) -> String {
        s.split(separator: "\n", omittingEmptySubsequences: true).first.map { String($0).trimmingCharacters(in: .whitespaces) } ?? ""
    }
}
