import Foundation
import HabAPI

/// Finds or starts the console of a session (app.md 6.1): a plain shell on
/// the same hab, filed in the session's project, so it starts in the
/// project's folder. It is a session of its own, named after the one it
/// belongs to, so it outlives the app and a second device finds it too.
public enum ConsoleOpener {
    /// The console's session name: "<session> console".
    public static func name(for session: Session) -> String {
        session.name + " console"
    }

    /// The pane of the session's running console, when there is one.
    public static func existing(in store: HabStore, for session: Session) -> String? {
        let want = name(for: session)
        guard let c = store.sessions.values.first(where: { $0.name == want }),
              c.state == .running, let pane = c.pane, store.panes[pane] != nil else { return nil }
        return pane
    }

    /// What the opener needs from the hab, so tests can count calls.
    public struct Calls: Sendable {
        public var store: @Sendable () async -> HabStore
        public var create: @Sendable (SessionCreateParams) async throws -> SessionCreated
        public var close: @Sendable (_ session: String) async throws -> Void

        public init(store: @escaping @Sendable () async -> HabStore,
                    create: @escaping @Sendable (SessionCreateParams) async throws -> SessionCreated,
                    close: @escaping @Sendable (String) async throws -> Void) {
            self.store = store
            self.create = create
            self.close = close
        }

        public init(connection: HabConnection) {
            store = { await connection.state.store }
            create = { try await connection.call(HabMethods.sessionCreate, $0) }
            close = { _ = try await connection.call(HabMethods.sessionClose, SessionRef(session: $0)) }
        }
    }

    public enum Failure: Error, CustomStringConvertible {
        case noSession
        public var description: String { "the session is gone" }
    }

    /// Returns the pane of the session's console, starting one when none
    /// runs. An exited console's record is removed first, so its name is free.
    public static func open(session id: String, calls: Calls) async throws -> String {
        let store = await calls.store()
        guard let s = store.sessions[id] else { throw Failure.noSession }
        if let pane = existing(in: store, for: s) { return pane }
        let want = name(for: s)
        if let old = store.sessions.values.first(where: { $0.name == want }) {
            try await calls.close(old.id)
        }
        let created = try await calls.create(SessionCreateParams(name: want, project: s.project))
        return created.pane
    }
}
