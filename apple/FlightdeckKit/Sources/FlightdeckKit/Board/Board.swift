import Foundation
import HabAPI
import Observation

public struct BoardSection: Identifiable, Hashable, Sendable {
    public var kind: BoardSectionKind
    public var rows: [BoardRow]
    public var id: String { kind.rawValue }
}

/// One merged model over every hab. Each hab's state comes from its own
/// connection; seq is never compared across habs (hab-logic-api.md 7.9).
@MainActor
@Observable
public final class Board {
    public struct Hab: Identifiable, Sendable, Equatable {
        public var id: String
        /// The label from config; nil uses the hab's host id.
        public var configLabel: String?
        public var endpoint: String
        public var state = HabState()

        /// The label every row of this hab shows.
        public var label: String { configLabel ?? state.welcome?.host.id ?? state.store.host ?? id }
    }

    public private(set) var habs: [Hab] = []
    public private(set) var sections: [BoardSection] = []

    /// Called when the owner opens a row. Chat and terminal views attach here.
    @ObservationIgnored public var onOpen: ((SessionTarget) -> Void)?

    @ObservationIgnored private var seen: Set<String> = []
    /// When each row entered its current rank, so a row moves only when its group changes.
    @ObservationIgnored private var entered: [String: (rank: BoardRank, at: Date)] = [:]
    @ObservationIgnored private let now: () -> Date

    public init(now: @escaping () -> Date = Date.init) {
        self.now = now
    }

    /// Adds a hab, in config order.
    public func register(id: String, label: String?, endpoint: String) {
        guard !habs.contains(where: { $0.id == id }) else { return }
        habs.append(Hab(id: id, configLabel: label, endpoint: endpoint))
        rebuild()
    }

    /// Replaces one hab's state.
    public func update(_ id: String, _ state: HabState) {
        guard let i = habs.firstIndex(where: { $0.id == id }) else { return }
        guard habs[i].state != state else { return }
        habs[i].state = state
        rebuild()
    }

    /// Opens a row: marks a `done` seen, then calls `onOpen`.
    public func open(_ row: BoardRow) {
        if row.state == .done, let k = row.seenKey {
            seen.insert(k)
            rebuild()
        }
        onOpen?(row.target)
    }

    public var rows: [BoardRow] { sections.flatMap(\.rows) }

    /// Sessions that need the owner, across online habs (the app badge).
    public var needsYouCount: Int {
        let online = Set(habs.filter { $0.state.status == .online }.map(\.id))
        return sections.first { $0.kind == .needsYou }?.rows.filter { online.contains($0.target.habID) }.count ?? 0
    }

    public var onlineCount: Int { habs.filter { $0.state.status == .online }.count }

    private func rebuild() {
        var all: [BoardRow] = []
        let t = now()
        for h in habs {
            // A hab whose revision does not fit shows "update needed"; its rows are hidden, not stale.
            if case .updateNeeded = h.state.status { continue }
            var rows = BoardRows.build(habID: h.id, host: h.label, store: h.state.store, seen: seen)
            if h.state.status == .offline {
                for i in rows.indices {
                    rows[i].offline = true
                    rows[i].offlineSince = h.state.lostAt
                }
            }
            all += rows
        }
        var keep: [String: (rank: BoardRank, at: Date)] = [:]
        for r in all {
            if let e = entered[r.id], e.rank == r.rank {
                keep[r.id] = e
            } else if entered[r.id] == nil {
                // First sight: order by the hab's own stamp when it has one.
                keep[r.id] = (r.rank, r.since ?? t)
            } else {
                keep[r.id] = (r.rank, t)
            }
        }
        entered = keep
        all.sort { a, b in
            if a.rank != b.rank { return a.rank < b.rank }
            let ea = keep[a.id]!.at, eb = keep[b.id]!.at
            if ea != eb { return ea > eb }
            return a.id < b.id
        }
        var out: [BoardSection] = []
        for kind in BoardSectionKind.allCases {
            let rows = all.filter { $0.rank.section == kind }
            if !rows.isEmpty { out.append(BoardSection(kind: kind, rows: rows)) }
        }
        sections = out
    }
}
