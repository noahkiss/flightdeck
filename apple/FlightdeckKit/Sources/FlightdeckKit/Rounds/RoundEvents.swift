import Foundation
import HabAPI

/// Applies the round events of rounds.md 13 to a hab's state.
enum RoundEvents {
    /// How many runs `HabState.roundRuns` holds; ended runs go first, oldest first.
    static let keepRuns = 200

    /// Applies `round.updated` or `round.run_updated`. Returns whether the state changed.
    ///
    /// A `round.updated` whose revision is not above the held one is ignored, as for any object
    /// within an epoch (hab-logic-api.md 3.5); a new epoch brings a new snapshot first. One with
    /// `deleted` removes the round. A `round.run_updated` is ephemeral and applies in seq order.
    static func apply(_ e: Event, to state: inout HabState) -> Bool {
        switch e.type {
        case "round.updated":
            guard var r = try? e.data.decode(Round.self) else { return false }
            let rev = e.revision ?? r.revision
            if let held = state.rounds[r.id], rev <= held.revision { return false }
            if r.deleted == true {
                return state.rounds.removeValue(forKey: r.id) != nil
            }
            r.revision = rev
            // Events come in seq order, so the round's copy of its run is current; only a
            // newer run held already stays.
            if let seen = state.rounds[r.id]?.lastRun, let new = r.lastRun, seen.number > new.number {
                r.lastRun = seen
            }
            state.rounds[r.id] = r
            return true
        case "round.run_updated":
            guard let run = try? e.data.decode(RoundRun.self) else { return false }
            var changed = false
            if state.roundRuns[run.id] != run {
                state.roundRuns[run.id] = run
                trim(&state.roundRuns)
                changed = true
            }
            if var r = state.rounds[run.round] {
                let newest = r.lastRun.map { $0.id == run.id || run.number > $0.number } ?? true
                if newest, r.lastRun != run {
                    r.lastRun = run
                    state.rounds[run.round] = r
                    changed = true
                }
            }
            return changed
        default:
            return false
        }
    }

    private static func trim(_ runs: inout [String: RoundRun]) {
        guard runs.count > keepRuns else { return }
        let order = runs.values.sorted { a, b in
            if a.state.isFinal != b.state.isFinal { return a.state.isFinal }
            return a.created < b.created
        }
        for r in order.prefix(runs.count - keepRuns) { runs[r.id] = nil }
    }
}

extension RoundRunState {
    /// The states a run ends in (rounds.md 7.1).
    public var isFinal: Bool {
        switch self {
        case .succeeded, .failed, .timedOut, .skipped, .takenOver: return true
        default: return false
        }
    }
}
