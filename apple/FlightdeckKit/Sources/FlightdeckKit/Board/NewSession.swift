import Foundation
import HabAPI

/// One project the New session form offers (app.md 3.10): a project of the hab's structure,
/// its catalog folders, or both matched by remote.
public struct NewSessionProject: Identifiable, Hashable, Sendable {
    /// The structure's project id, else "folder:" and its first folder.
    public var id: String
    public var name: String
    /// The hab's project id, when the structure holds the project.
    public var projectID: String?
    /// Its working folders from the hab's catalog, in the catalog's order. Empty when the
    /// catalog does not list it.
    public var folders: [String]

    public init(id: String, name: String, projectID: String?, folders: [String]) {
        self.id = id
        self.name = name
        self.projectID = projectID
        self.folders = folders
    }
}

/// What the New session form offers on one hab (app.md 3.10).
public struct NewSessionChoices: Equatable, Sendable {
    public var projects: [NewSessionProject]
    /// Harness ids to offer, in a fixed order; the shell is always offered besides.
    public var harnesses: [String]

    /// The harnesses Flightdeck has adapters for, in the order the form lists them.
    public static let supported = ["claude", "codex", "opencode"]

    public init(projects: [NewSessionProject], harnesses: [String]) {
        self.projects = projects
        self.harnesses = harnesses
    }

    /// The choices from the hab's structure and, when read, its catalog. A catalog project
    /// and a structure project are one when they share a remote, else a root commit. With no
    /// catalog the form offers every supported harness and lets the hab refuse a missing one;
    /// with a catalog, only the supported harnesses it found on the host.
    public static func make(store: HabStore, catalog: Catalog?) -> NewSessionChoices {
        var out: [NewSessionProject] = []
        var used: Set<String> = []
        for c in catalog?.projects ?? [] {
            let folders = c.folders.map(\.path)
            guard let first = folders.first else { continue }
            let match = store.projects.values.first { p in
                if let r = c.remote, !r.isEmpty { return p.remote == r }
                if let rc = c.rootCommit, !rc.isEmpty { return p.rootCommit == rc }
                return false
            }
            if let m = match {
                if used.contains(m.id) { continue }
                used.insert(m.id)
                out.append(NewSessionProject(id: m.id, name: m.name, projectID: m.id, folders: folders))
            } else {
                out.append(NewSessionProject(id: "folder:" + first, name: c.name, projectID: nil, folders: folders))
            }
        }
        for p in store.projects.values where !used.contains(p.id) {
            out.append(NewSessionProject(id: p.id, name: p.name, projectID: p.id, folders: []))
        }
        out.sort { a, b in
            let o = a.name.localizedCaseInsensitiveCompare(b.name)
            return o == .orderedSame ? a.id < b.id : o == .orderedAscending
        }
        let harnesses: [String]
        if let catalog {
            let found = Set(catalog.harnesses.map(\.name))
            harnesses = supported.filter { found.contains($0) }
        } else {
            harnesses = supported
        }
        return NewSessionChoices(projects: out, harnesses: harnesses)
    }
}

/// The New session form's fields (app.md 3.10). The hab checks every rule again; the form
/// checks only what it needs to build `SessionCreateParams`.
public struct NewSessionDraft: Equatable, Sendable {
    /// Where the session starts.
    public enum Place: Hashable, Sendable {
        /// No project: the hab's home folder, unfiled.
        case home
        /// A project the form offers, by `NewSessionProject.id`.
        case project(String)
        /// A folder the owner types.
        case folder
    }

    public var habID: String
    public var place: Place = .home
    /// The project's folder; nil takes its first, or lets the hab pick when it has none.
    public var folder: String?
    /// The typed absolute folder, for `.folder`.
    public var typedFolder = ""
    /// The harness to start; nil starts the hab's shell.
    public var harness: String? = "claude"
    /// One key per form, so a retried Start starts one session (hab-logic-api.md 3.9). The
    /// form makes a new one when a field changes, so a changed form starts its own session.
    public var key: String

    public init(habID: String, key: String = UUID().uuidString) {
        self.habID = habID
        self.key = key
    }

    /// The draft for a hab with these choices: its first harness, and the place kept when the
    /// hab offers it.
    public mutating func fit(_ c: NewSessionChoices) {
        if let h = harness, !c.harnesses.contains(h) { harness = c.harnesses.first }
        if case .project(let id) = place, let p = c.projects.first(where: { $0.id == id }) {
            if let f = folder, !p.folders.contains(f) { folder = nil }
        } else if case .project = place {
            place = .home
            folder = nil
        }
    }

    /// The call's parameters, or the first thing missing. `keys` says whether the hab takes
    /// an idempotency key (capability `idempotency-keys`).
    public func params(_ c: NewSessionChoices, keys: Bool) -> Result<SessionCreateParams, NewSessionError> {
        var p = SessionCreateParams()
        switch place {
        case .home:
            break
        case .project(let id):
            guard let proj = c.projects.first(where: { $0.id == id }) else {
                return .failure(NewSessionError("Choose a project."))
            }
            if let dir = folder ?? proj.folders.first {
                p.projectDir = dir
            } else if let pid = proj.projectID {
                // The hab starts in the project's most recently used folder.
                p.project = pid
            } else {
                return .failure(NewSessionError("Choose a folder."))
            }
        case .folder:
            let dir = typedFolder.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !dir.isEmpty else { return .failure(NewSessionError("Write the folder to start in.")) }
            guard dir.hasPrefix("/") else {
                return .failure(NewSessionError("Write the full path of the folder, starting with /."))
            }
            p.projectDir = dir
        }
        p.harness = harness
        if keys { p.idempotencyKey = key }
        return .success(p)
    }
}

public struct NewSessionError: Error, CustomStringConvertible, Equatable, Sendable {
    public var description: String
    public init(_ d: String) { description = d }
}

/// A hab the New session form can start on.
public struct NewSessionHab: Identifiable, Hashable, Sendable {
    public var id: String
    public var label: String
}

/// Starts sessions from the app (app.md 3.10): reads each hab's catalog for the form, calls
/// `session.create`, and asks the shell to open the new session once the board lists it.
@MainActor
@Observable
public final class NewSessionCenter {
    /// The welcome capability that says a hab serves `catalog.get`.
    public nonisolated static let catalogCapability = "catalog"
    /// The welcome capability that says a hab takes idempotency keys.
    public nonisolated static let keysCapability = "idempotency-keys"

    /// The session a start made, until the shell opens its row.
    public private(set) var pending: SessionTarget?
    /// When `pending` was set. A row that takes longer than `followLimit` to show does not
    /// jump into view.
    @ObservationIgnored private var pendingAt = Date.distantPast
    static let followLimit: TimeInterval = 60
    /// The clock `pendingAt` is timed by; tests move it.
    @ObservationIgnored var now: @MainActor () -> Date = { Date() }
    /// Each hab's catalog, once read; a hab without the catalog keeps none.
    public private(set) var catalogs: [String: Catalog] = [:]
    /// Habs whose catalog is being read.
    public private(set) var loading: Set<String> = []

    @ObservationIgnored private let board: Board
    @ObservationIgnored private let connection: @MainActor (String) -> HabConnection?

    public init(board: Board, connection: @escaping @MainActor (String) -> HabConnection?) {
        self.board = board
        self.connection = connection
    }

    /// The online habs, in the app's order. A hab that needs an update starts nothing.
    public var habs: [NewSessionHab] {
        board.habs.filter { $0.state.status == .online }.map { NewSessionHab(id: $0.id, label: $0.label) }
    }

    /// A fresh form on the first online hab; nil while no hab is online.
    public func draft() -> NewSessionDraft? {
        habs.first.map { NewSessionDraft(habID: $0.id) }
    }

    /// What the form offers on a hab now: its structure, and its catalog once read.
    public func choices(habID: String) -> NewSessionChoices {
        let store = board.habs.first { $0.id == habID }?.state.store ?? HabStore()
        return NewSessionChoices.make(store: store, catalog: catalogs[habID])
    }

    /// Reads the hab's catalog for its project folders and harnesses. A hab without the
    /// catalog, or a failed read, leaves the structure's projects and every harness.
    public func load(habID: String) async {
        guard let conn = connection(habID), await conn.has(capability: Self.catalogCapability) else { return }
        loading.insert(habID)
        defer { loading.remove(habID) }
        if let r = try? await conn.call(HabMethods.catalogGet, CatalogGetParams()), let c = r.catalog {
            catalogs[habID] = c
        }
    }

    /// Starts the draft's session. Returns once the hab answers; the shell opens the session
    /// when its row shows. Throws a sentence for the form.
    @discardableResult
    public func start(_ draft: NewSessionDraft) async throws -> SessionTarget {
        guard let conn = connection(draft.habID) else { throw NewSessionError("That computer is no longer in the app.") }
        let p: SessionCreateParams
        let keys = await conn.has(capability: Self.keysCapability)
        switch draft.params(choices(habID: draft.habID), keys: keys) {
        case .failure(let e): throw e
        case .success(let v): p = v
        }
        do {
            let r = try await conn.call(HabMethods.sessionCreate, p)
            let t = SessionTarget(habID: draft.habID, session: r.session, pane: r.pane)
            pending = t
            pendingAt = now()
            return t
        } catch {
            throw NewSessionError(Self.sentence(error))
        }
    }

    /// The board row of the started session, once the board lists it.
    public func pendingRow(in rows: [BoardRow]) -> BoardRow? {
        guard let t = pending, now().timeIntervalSince(pendingAt) <= Self.followLimit else { return nil }
        return rows.first { $0.target.habID == t.habID && $0.target.session == t.session }
    }

    /// The shell opened the started session, or the owner opened another.
    public func opened() { pending = nil }

    /// The hab's refusal as a sentence for the form. The hab names its field
    /// ("project_dir /x is not inside a git repository"); the form shows the folder alone.
    static func sentence(_ error: Error) -> String {
        var t = RoundCenter.text(error)
        if t.hasPrefix("project_dir ") { t.removeFirst("project_dir ".count) }
        return RoundCenter.sentence(t)
    }
}
