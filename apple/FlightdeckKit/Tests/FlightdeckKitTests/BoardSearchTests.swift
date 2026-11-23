import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The board's search (app.md 3.6).
@MainActor
final class BoardSearchTests: XCTestCase {
    private let rows = [
        boardRow("bench/s/s1", title: "Fix the tide tables", name: "tides", project: "tide-tables", harness: "claude", model: "opus"),
        boardRow("bench/s/s2", session: "s2", title: "Café menu", name: "menu", project: "paper-lantern", harness: "codex"),
        boardRow("lab/s/s3", hab: "lab", session: "s3", title: "zsh", name: "zsh", project: "tide-tables"),
    ]

    private var sections: [BoardSection] { [BoardSection(kind: .done, rows: rows)] }

    private func ids(_ q: String, _ scope: BoardSearchScope = .all) -> [String] {
        BoardSearch.filter(sections, query: q, scope: scope).flatMap(\.rows).map(\.id)
    }

    func testMatchesNamesTheAppHolds() {
        XCTAssertEqual(ids(""), rows.map(\.id), "an empty query filters nothing")
        XCTAssertEqual(ids("TIDE"), ["bench/s/s1", "lab/s/s3"], "any case, title or project")
        XCTAssertEqual(ids("cafe"), ["bench/s/s2"], "accents ignored")
        XCTAssertEqual(ids("claude code"), ["bench/s/s1"], "the harness by its name")
        XCTAssertEqual(ids("opus"), ["bench/s/s1"], "the model")
        XCTAssertEqual(ids("lab"), ["lab/s/s3"], "the hab")
        XCTAssertEqual(ids("tide zsh"), ["lab/s/s3"], "every word must match")
        XCTAssertEqual(ids("nothing-like-this"), [])
        XCTAssertTrue(BoardSearch.filter(sections, query: "nothing", scope: .all).isEmpty, "an emptied section goes")
    }

    func testHostScope() {
        XCTAssertEqual(ids("", .hab("lab")), ["lab/s/s3"])
        XCTAssertEqual(ids("tide", .hab("bench")), ["bench/s/s1"])
        XCTAssertFalse(BoardSearch.isActive(query: "  ", scope: .all))
        XCTAssertTrue(BoardSearch.isActive(query: "", scope: .hab("lab")))
        XCTAssertEqual(BoardSearch.fit(.hab("gone"), habs: ["bench"]), .all, "a hab that left falls back to All")
        XCTAssertEqual(BoardSearch.fit(.hab("bench"), habs: ["bench"]), .hab("bench"))
    }

    func testSessionsListCountsFollowTheFilter() {
        let list = SessionsList.build(sections: sections, confirms: [], groups: [
            GroupSummary(id: "lab/g/g1", title: "Docs sweep", habLabel: "lab", members: 2, working: 1, waiting: 1,
                         over: false, since: nil, memberTargets: []),
        ])
        let got = BoardSearch.filter(list, query: "tide", scope: .all)
        XCTAssertEqual(got.map(\.title), ["Done · 2"])
        let groups = BoardSearch.filter(list, query: "docs", scope: .hab("lab"))
        XCTAssertEqual(groups.flatMap(\.items).map(\.id), ["lab/g/g1"], "a group matches on its title and hab")
        XCTAssertTrue(BoardSearch.filter(list, query: "docs", scope: .hab("bench")).isEmpty)
    }
}
