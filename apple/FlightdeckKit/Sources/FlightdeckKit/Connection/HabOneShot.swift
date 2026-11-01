import Foundation
import HabAPI

/// One call to a hab on a connection of its own: open, hello, the call,
/// close. For work outside the visible app, such as a notification action in
/// the background, where no `HabConnection` runs. Fails fast and never
/// retries (app.md 18.7): a stale retry could act on something the owner no
/// longer wants.
public enum HabOneShot {
    public struct Result<R: Sendable>: Sendable {
        public var value: R
        /// From the open to the call's response.
        public var elapsed: Duration
    }

    /// Runs `method` once. `deadline` bounds the whole exchange; iOS gives a
    /// notification action about 30 s.
    public static func call<P: Encodable & Sendable, R: Decodable & Sendable>(
        _ transport: Transport,
        _ method: HabMethod<P, R>,
        _ params: P,
        clientLabel: String? = nil,
        clientVersion: String = "0",
        deadline: Duration = .seconds(20)
    ) async throws -> Result<R> {
        let clock = ContinuousClock()
        let start = clock.now
        return try await withThrowingTaskGroup(of: Result<R>.self) { group in
            group.addTask {
                let stream = try await transport.open(.control)
                let rpc = RPCClient(stream: stream)
                await rpc.start()
                do {
                    let hello = HelloParams(
                        client: ClientInfo(kind: .app, name: "Flightdeck", version: clientVersion),
                        label: clientLabel,
                        minRevision: HabAPI.minRevision,
                        reconnectToken: nil,
                        revision: HabAPI.revision
                    )
                    let left = deadline - (clock.now - start)
                    _ = try await rpc.call(HabMethods.habHello, hello, timeout: left)
                    let r = try await rpc.call(method, params, timeout: deadline - (clock.now - start))
                    await rpc.close()
                    return Result(value: r, elapsed: clock.now - start)
                } catch {
                    await rpc.close()
                    throw error
                }
            }
            group.addTask {
                try await Task.sleep(for: deadline)
                throw TransportError("no answer within \(deadline.components.seconds) s")
            }
            defer { group.cancelAll() }
            guard let r = try await group.next() else { throw CancellationError() }
            return r
        }
    }
}
