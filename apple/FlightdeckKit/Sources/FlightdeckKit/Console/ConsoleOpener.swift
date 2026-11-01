import Foundation
import HabAPI

/// Finds or starts the console of a session (app.md 6.1): a plain shell on
/// the same hab, started in the session's folder. It is a session of its
/// own, named after the one it belongs to, so it outlives the app and a
/// second device finds it too.
public enum ConsoleOpener {
    /// The console's session name: "<session> console".
    public static func name(for session: Session) -> String {
        session.name + " console"
    }

    /// Whether `session` is some other session's console in `store`.
    public static func isConsole(_ session: Session, in store: HabStore) -> Bool {
        store.sessions.values.contains { $0.id != session.id && name(for: $0) == session.name }
    }

    /// The pane of the session's running console, when there is one. A
    /// running console counts even before its pane's own event arrived.
    public static func existing(in store: HabStore, for session: Session) -> String? {
        let want = name(for: session)
        guard let c = store.sessions.values.first(where: { $0.name == want }),
              c.state == .running, let pane = c.pane else { return nil }
        return pane
    }

    /// What the opener needs from the hab, so tests can count calls.
    public struct Calls: Sendable {
        public var store: @Sendable () async -> HabStore
        public var create: @Sendable (SessionCreateParams) async throws -> SessionCreated
        public var close: @Sendable (_ session: String) async throws -> Void
        /// The folder of the pane's newest command, when the shell reported one.
        public var newestCwd: @Sendable (_ pane: String) async -> String?

        public init(store: @escaping @Sendable () async -> HabStore,
                    create: @escaping @Sendable (SessionCreateParams) async throws -> SessionCreated,
                    close: @escaping @Sendable (String) async throws -> Void,
                    newestCwd: @escaping @Sendable (String) async -> String? = { _ in nil }) {
            self.store = store
            self.create = create
            self.close = close
            self.newestCwd = newestCwd
        }

        public init(connection: HabConnection) {
            store = { await connection.state.store }
            create = { try await connection.call(HabMethods.sessionCreate, $0) }
            close = { _ = try await connection.call(HabMethods.sessionClose, SessionRef(session: $0)) }
            newestCwd = { pane in
                // A hab without the commands capability, or a pane with no marks, gives none.
                let page = try? await connection.call(HabMethods.commandList, CommandListParams(limit: 1, pane: pane))
                return page?.commands.last?.cwd
            }
        }
    }

    public enum Failure: Error, CustomStringConvertible {
        case noSession
        case starting
        public var description: String {
            switch self {
            case .noSession: return "the session is gone"
            case .starting: return "the console is still starting; try again in a moment"
            }
        }
    }

    /// Returns the pane of the session's console, starting one when none
    /// runs. Only a console that no longer runs has its record removed, so
    /// its name is free; a running shell is never closed. The new shell
    /// starts in the folder of the session's newest command when the shell
    /// reported one; otherwise the hab picks the project's folder (app.md 6.3).
    public static func open(session id: String, calls: Calls) async throws -> String {
        let store = await calls.store()
        guard let s = store.sessions[id] else { throw Failure.noSession }
        if let pane = existing(in: store, for: s) { return pane }
        let want = name(for: s)
        if let old = store.sessions.values.first(where: { $0.name == want }) {
            guard old.state != .running else { throw Failure.starting }
            try await calls.close(old.id)
        }
        var cwd: String?
        if let pane = s.pane { cwd = await calls.newestCwd(pane) }
        do {
            do {
                return try await calls.create(SessionCreateParams(cwd: cwd, name: want, project: s.project)).pane
            } catch where (error as? RPCError)?.kind == .invalid && cwd != nil {
                // The folder is gone since that command ran: let the hab pick.
                return try await calls.create(SessionCreateParams(name: want, project: s.project)).pane
            }
        } catch let e as RPCError where e.kind == .conflict {
            // Another open (a double tap, a second device) started it first.
            if let pane = existing(in: await calls.store(), for: s) { return pane }
            throw e
        }
    }
}
