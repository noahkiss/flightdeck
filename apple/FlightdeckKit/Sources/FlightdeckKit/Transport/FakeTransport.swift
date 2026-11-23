import Foundation
import Synchronization

/// An in-memory transport for tests and demo mode. Each `open` makes a pair of
/// connected streams: the caller gets one end, `accept` gets the other (a fake
/// hab serves it). Closing either end ends both, like a socket.
public final class FakeTransport: Transport, Sendable {
    public typealias Accept = @Sendable (_ server: ByteStream, _ kind: StreamKind) -> Void

    public let endpoint: String
    private let accept: Accept

    private struct Counts {
        var failures = 0
        var refusing = false
        var live = 0
        var opens = 0
    }

    private let counts = Mutex(Counts())

    public init(endpoint: String = "fake", accept: @escaping Accept) {
        self.endpoint = endpoint
        self.accept = accept
    }

    /// Makes the next `n` opens fail.
    public func failNext(_ n: Int) {
        counts.withLock { $0.failures += n }
    }

    /// While set, every open fails, as for a host that is down.
    public func setRefusing(_ on: Bool) {
        counts.withLock { $0.refusing = on }
    }

    /// Client ends opened and not yet closed.
    public var openStreams: Int {
        counts.withLock { $0.live }
    }

    /// Opens attempted, including failed ones.
    public var openAttempts: Int {
        counts.withLock { $0.opens }
    }

    public func open(_ kind: StreamKind) async throws -> ByteStream {
        let refused: Bool = counts.withLock { c in
            c.opens += 1
            if c.refusing || c.failures > 0 {
                if c.failures > 0 { c.failures -= 1 }
                return true
            }
            c.live += 1
            return false
        }
        if refused {
            throw TransportError("fake: connection refused")
        }
        let (client, server) = FakeStream.pair { [weak self] in
            self?.counts.withLock { $0.live -= 1 }
        }
        accept(server, kind)
        return client
    }
}

/// One end of an in-memory stream pair.
public final class FakeStream: ByteStream, Sendable {
    private let shared: Shared
    private let side: Int

    final class Shared: Sendable {
        struct State {
            var buffers: [[Data]] = [[], []]
            var waiters: [CheckedContinuation<Data?, Error>?] = [nil, nil]
            var closed = false
        }

        let state = Mutex(State())
        let onClose: @Sendable () -> Void
        init(onClose: @escaping @Sendable () -> Void) { self.onClose = onClose }
    }

    private init(shared: Shared, side: Int) {
        self.shared = shared
        self.side = side
    }

    /// A connected pair: (client end, server end). `onClose` runs once, when
    /// the pair closes.
    public static func pair(onClose: @escaping @Sendable () -> Void = {}) -> (FakeStream, FakeStream) {
        let s = Shared(onClose: onClose)
        return (FakeStream(shared: s, side: 0), FakeStream(shared: s, side: 1))
    }

    public func read() async throws -> Data? {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Data?, Error>) in
            enum Next { case data(Data), end, wait }
            let side = side
            let next: Next = shared.state.withLock { s in
                if !s.buffers[side].isEmpty { return .data(s.buffers[side].removeFirst()) }
                if s.closed { return .end }
                s.waiters[side] = c
                return .wait
            }
            switch next {
            case .data(let d): c.resume(returning: d)
            case .end: c.resume(returning: nil)
            case .wait: break
            }
        }
    }

    public func write(_ data: Data) async throws {
        try send(data)
    }

    /// Writes without suspending, so a caller on an actor keeps its order:
    /// nothing else on that actor runs between deciding a message and sending it.
    public func send(_ data: Data) throws {
        let peer = 1 - side
        enum Outcome { case closed, buffered, wake(CheckedContinuation<Data?, Error>) }
        let outcome: Outcome = shared.state.withLock { s in
            if s.closed { return .closed }
            if let w = s.waiters[peer] {
                s.waiters[peer] = nil
                return .wake(w)
            }
            s.buffers[peer].append(data)
            return .buffered
        }
        switch outcome {
        case .closed: throw TransportError("fake: stream closed")
        case .buffered: break
        case .wake(let w): w.resume(returning: data)
        }
    }

    public func close() {
        let side = side
        let ws: [CheckedContinuation<Data?, Error>?]? = shared.state.withLock { s in
            if s.closed { return nil }
            s.closed = true
            let ws = s.waiters
            s.waiters = [nil, nil]
            // Bytes already sent stay readable for the other side, as on a socket;
            // this side drops what it had not read.
            s.buffers[side].removeAll()
            return ws
        }
        guard let ws else { return }
        for w in ws { w?.resume(returning: nil) }
        shared.onClose()
    }
}
