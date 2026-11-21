import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// New session (app.md 3.10): the form's choices from the structure and the catalog, the
/// call it builds, and a start over the fake hab that opens the new session's row.
@MainActor
final class NewSessionTests: XCTestCase {
    private func store(_ projects: [Project]) -> HabStore {
        HabStore(StructureResult(agents: [], host: HostInfo(id: "bench"), panes: [], projects: projects, seq: 1, sessions: []))
    }

    private func catalog(harnesses: [String], projects: [CatalogProject]) -> Catalog {
        Catalog(actions: [], agents: [], at: Fixture.stamp(Date()), capabilities: [], deviceId: "d", generation: 1,
                harnesses: harnesses.map { CatalogTool(name: $0) }, host: "bench", peers: [], projects: projects,
                services: [], system: CatalogSystem(arch: "arm64", cores: 8, os: "darwin"), toolchains: [], watches: [])
    }

    private func folders(_ paths: String...) -> [CatalogFolder] { paths.map { CatalogFolder(path: $0) } }

    // MARK: - Choices

    func testChoicesMergeStructureAndCatalog() {
        let s = store([
            Fixture.project("p1", name: "tide-tables"),
            Fixture.project("p2", name: "Seed-catalog"),
        ])
        let c = catalog(harnesses: ["codex", "claude", "pi"], projects: [
            CatalogProject(folders: folders("/work/tide-tables", "/work/tide-tables-wt"), name: "tide-tables",
                           remote: "https://example.com/demo/tide-tables.git"),
            CatalogProject(folders: folders("/work/paper-lantern"), name: "paper-lantern", rootCommit: "abc"),
            CatalogProject(folders: [], name: "empty"),
        ])
        let got = NewSessionChoices.make(store: s, catalog: c)
        XCTAssertEqual(got.projects.map(\.name), ["paper-lantern", "Seed-catalog", "tide-tables"],
                       "by name, any case; a catalog project with no folder is left out")
        XCTAssertEqual(got.projects[2].id, "p1", "a catalog project with the structure's remote is that project")
        XCTAssertEqual(got.projects[2].folders, ["/work/tide-tables", "/work/tide-tables-wt"])
        XCTAssertEqual(got.projects[0].id, "folder:/work/paper-lantern", "a project only the catalog has")
        XCTAssertNil(got.projects[0].projectID)
        XCTAssertEqual(got.projects[1].folders, [], "a project only the structure has: the hab picks its folder")
        XCTAssertEqual(got.harnesses, ["claude", "codex"], "supported harnesses the host has, in a fixed order")
    }

    func testChoicesWithoutCatalogOfferEveryHarness() {
        let got = NewSessionChoices.make(store: store([Fixture.project("p1", name: "tide-tables")]), catalog: nil)
        XCTAssertEqual(got.harnesses, NewSessionChoices.supported)
        XCTAssertEqual(got.projects.map(\.id), ["p1"])
        let none = NewSessionChoices.make(store: HabStore(), catalog: catalog(harnesses: [], projects: []))
        XCTAssertEqual(none.harnesses, [], "a catalog with no agent offers the shell only")
    }

    // MARK: - The call

    func testParams() throws {
        let c = NewSessionChoices(projects: [
            NewSessionProject(id: "p1", name: "tide-tables", projectID: "p1", folders: ["/work/a", "/work/b"]),
            NewSessionProject(id: "p2", name: "seed", projectID: "p2", folders: []),
        ], harnesses: ["claude"])
        var d = NewSessionDraft(habID: "bench", key: "k1")

        var p = try d.params(c, keys: true).get()
        XCTAssertNil(p.projectDir)
        XCTAssertNil(p.project)
        XCTAssertEqual(p.harness, "claude")
        XCTAssertEqual(p.idempotencyKey, "k1")
        XCTAssertNil(try d.params(c, keys: false).get().idempotencyKey, "no key to a hab that does not take one")

        d.place = .project("p1")
        XCTAssertEqual(try d.params(c, keys: false).get().projectDir, "/work/a", "the first folder by default")
        d.folder = "/work/b"
        XCTAssertEqual(try d.params(c, keys: false).get().projectDir, "/work/b")

        d.place = .project("p2")
        d.folder = nil
        p = try d.params(c, keys: false).get()
        XCTAssertEqual(p.project, "p2", "no folder known: the hab starts in the project's last folder")
        XCTAssertNil(p.projectDir)

        d.place = .project("gone")
        XCTAssertEqual(d.params(c, keys: false).failure, NewSessionError("Choose a project."))

        d.place = .folder
        XCTAssertEqual(d.params(c, keys: false).failure, NewSessionError("Write the folder to start in."))
        d.typedFolder = "work/c"
        XCTAssertEqual(d.params(c, keys: false).failure, NewSessionError("Write the full path of the folder, starting with /."))
        d.typedFolder = " /work/c \n"
        XCTAssertEqual(try d.params(c, keys: false).get().projectDir, "/work/c")

        d.harness = nil
        XCTAssertNil(try d.params(c, keys: false).get().harness, "the shell: no harness")
    }

    func testFitKeepsWhatTheHabOffers() {
        let c = NewSessionChoices(projects: [
            NewSessionProject(id: "p1", name: "tide-tables", projectID: "p1", folders: ["/work/a"]),
        ], harnesses: ["codex"])
        var d = NewSessionDraft(habID: "bench")
        d.place = .project("p1")
        d.folder = "/elsewhere"
        d.fit(c)
        XCTAssertEqual(d.harness, "codex", "a harness the host lacks gives way to its first")
        XCTAssertEqual(d.place, .project("p1"))
        XCTAssertNil(d.folder, "a folder the project lacks is cleared")
        d.place = .project("p9")
        d.fit(c)
        XCTAssertEqual(d.place, .home, "a project the hab lacks goes back to none")
        d.harness = nil
        d.fit(c)
        XCTAssertNil(d.harness, "the shell stays")
    }

    // MARK: - Over the fake hab

    func testStartOpensTheNewSession() async throws {
        let hab = await sampleHab("bench")
        await hab.setCapability(NewSessionCenter.catalogCapability, true)
        await hab.setCapability(NewSessionCenter.keysCapability, true)
        await hab.setCatalog(harnesses: [CatalogTool(name: "claude"), CatalogTool(name: "codex")], projects: [
            CatalogProject(folders: folders("/work/tide-tables"), name: "tide-tables",
                           remote: "https://example.com/demo/tide-tables.git"),
        ])
        let f = Fleet()
        f.add(HabConnection(id: "bench", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        let center = f.newSessions

        var d = try XCTUnwrap(center.draft())
        XCTAssertEqual(d.habID, "bench")
        await center.load(habID: "bench")
        let c = center.choices(habID: "bench")
        XCTAssertEqual(c.harnesses, ["claude", "codex"])
        XCTAssertEqual(c.projects.first?.folders, ["/work/tide-tables"])
        d.place = .project("p1")
        d.harness = "codex"

        let t = try await center.start(d)
        let sent = await hab.sessionCreates
        XCTAssertEqual(sent.count, 1)
        XCTAssertEqual(sent.first?.projectDir, "/work/tide-tables")
        XCTAssertEqual(sent.first?.harness, "codex")
        XCTAssertEqual(sent.first?.idempotencyKey, d.key)
        XCTAssertEqual(center.pending, t)
        await eventually("the new row") { center.pendingRow(in: f.board.rows) != nil }
        XCTAssertEqual(center.pendingRow(in: f.board.rows)?.target.session, t.session)
        center.opened()
        XCTAssertNil(center.pending)

        // A retried Start with the same form starts nothing new.
        let again = try await center.start(d)
        XCTAssertEqual(again.session, t.session)
        let creates = await hab.sessionCreates.count
        XCTAssertEqual(creates, 2, "the hab saw the retry and started nothing")

        // A row that takes longer than the limit to show does not jump into view.
        let later = Date().addingTimeInterval(NewSessionCenter.followLimit + 1)
        center.now = { later }
        XCTAssertNil(center.pendingRow(in: f.board.rows))
        center.now = { Date() }
        center.opened()

        // A refusal comes back as a sentence for the form.
        do {
            try await center.start(NewSessionDraft(habID: "bench", key: "other").with { $0.place = .folder; $0.typedFolder = "/nowhere" })
            XCTFail("a folder outside a repository starts nothing")
        } catch let e as NewSessionError {
            XCTAssertEqual(e.description, "/nowhere is not inside a git repository.")
        }
    }
}

private extension NewSessionDraft {
    func with(_ f: (inout NewSessionDraft) -> Void) -> NewSessionDraft {
        var d = self
        f(&d)
        return d
    }
}

private extension Result {
    var failure: Failure? {
        if case .failure(let e) = self { return e }
        return nil
    }
}
