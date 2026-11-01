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
        let work = Task { () throws -> Result<R> in
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
                _ = try await rpc.call(HabMethods.habHello, hello, timeout: deadline - (clock.now - start))
                let r = try await rpc.call(method, params, timeout: deadline - (clock.now - start))
                await rpc.close()
                return Result(value: r, elapsed: clock.now - start)
            } catch {
                await rpc.close()
                throw error
            }
        }
        // The deadline holds even while a dial waits on a TCP connect, which
        // does not answer cancellation: on timeout this returns at once,
        // cancels the work and drops the carrier's connections.
        let race = OneShotRace<Result<R>>()
        return try await withTaskCancellationHandler {
            try await withCheckedThrowingContinuation { (c: CheckedContinuation<Result<R>, Error>) in
                race.begin(c)
                Task {
                    do { race.finish(.success(try await work.value)) } catch { race.finish(.failure(error)) }
                }
                Task {
                    try? await Task.sleep(for: deadline)
                    if race.finish(.failure(TransportError("no answer within \(deadline.components.seconds) s"))) {
                        work.cancel()
                        await transport.reset()
                    }
                }
            }
        } onCancel: {
            work.cancel()
            race.finish(.failure(CancellationError()))
        }
    }
}

/// The first outcome wins; later ones are dropped.
final class OneShotRace<T: Sendable>: @unchecked Sendable {
    private let lock = NSLock()
    private var cont: CheckedContinuation<T, Error>?
    private var early: Swift.Result<T, Error>?
    private var done = false

    func begin(_ c: CheckedContinuation<T, Error>) {
        let r: Swift.Result<T, Error>? = lock.withLock {
            if let e = early {
                early = nil
                return e
            }
            cont = c
            return nil
        }
        if let r { c.resume(with: r) }
    }

    /// Returns true when this outcome won.
    @discardableResult
    func finish(_ r: Swift.Result<T, Error>) -> Bool {
        let (won, c): (Bool, CheckedContinuation<T, Error>?) = lock.withLock {
            if done { return (false, nil) }
            done = true
            if cont == nil { early = r }
            let c = cont
            cont = nil
            return (true, c)
        }
        c?.resume(with: r)
        return won
    }
}
