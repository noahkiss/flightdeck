import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// A board row for tests that need no hab.
func boardRow(_ id: String, hab: String = "bench", session: String? = "s1", pane: String? = "%1", title: String = "claude",
                 name: String? = "claude", project: String? = nil, harness: String? = nil, model: String? = nil,
                 state: AgentStatus? = .idle, rank: BoardRank = .atRest) -> BoardRow {
    var r = BoardRow(id: id, target: SessionTarget(habID: hab, session: session, pane: pane), host: hab, title: title,
                     project: project, model: model, harness: harness, state: state, rank: rank)
    r.name = name
    return r
}

/// A defaults suite of its own, emptied before and after the test.
func emptySuite(_ name: String) -> UserDefaults {
    let d = UserDefaults(suiteName: name)!
    d.removePersistentDomain(forName: name)
    return d
}
