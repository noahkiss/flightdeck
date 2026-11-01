import Foundation

/// An in-memory transport for tests and demo mode. Each `open` makes a pair of
/// connected streams: the caller gets one end, `accept` gets the other (a fake
/// hab serves it). Closing either end ends both, like a socket.
public final class FakeTransport: Transport, @unchecked Sendable {
    public typealias Accept = @Sendable (_ server: ByteStream, _ kind: StreamKind) -> Void

    public let endpoint: String
    private let accept: Accept
    private let lock = NSLock()
    private var failures = 0
    private var refusing = false
    private var live = 0
    private var opens = 0

    public init(endpoint: String = "fake", accept: @escaping Accept) {
        self.endpoint = endpoint
        self.accept = accept
    }

    /// Makes the next `n` opens fail.
    public func failNext(_ n: Int) {
        lock.lock(); failures += n; lock.unlock()
    }

    /// While set, every open fails, as for a host that is down.
    public func setRefusing(_ on: Bool) {
        lock.lock(); refusing = on; lock.unlock()
    }

    /// Client ends opened and not yet closed.
    public var openStreams: Int {
        lock.lock(); defer { lock.unlock() }
        return live
    }

    /// Opens attempted, including failed ones.
    public var openAttempts: Int {
        lock.lock(); defer { lock.unlock() }
        return opens
    }

    public func open(_ kind: StreamKind) async throws -> ByteStream {
        let refused: Bool = lock.withLock {
            opens += 1
            if refusing || failures > 0 {
                if failures > 0 { failures -= 1 }
                return true
            }
            live += 1
            return false
        }
        if refused {
            throw TransportError("fake: connection refused")
        }
        let (client, server) = FakeStream.pair { [weak self] in
            guard let self else { return }
            self.lock.lock(); self.live -= 1; self.lock.unlock()
        }
        accept(server, kind)
        return client
    }
}

/// One end of an in-memory stream pair.
public final class FakeStream: ByteStream, @unchecked Sendable {
    private let shared: Shared
    private let side: Int

    final class Shared: @unchecked Sendable {
        let lock = NSLock()
        var buffers: [[Data]] = [[], []]
        var waiters: [CheckedContinuation<Data?, Error>?] = [nil, nil]
        var closed = false
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
            let s = shared
            s.lock.lock()
            if !s.buffers[side].isEmpty {
                let d = s.buffers[side].removeFirst()
                s.lock.unlock()
                c.resume(returning: d)
            } else if s.closed {
                s.lock.unlock()
                c.resume(returning: nil)
            } else {
                s.waiters[side] = c
                s.lock.unlock()
            }
        }
    }

    public func write(_ data: Data) async throws {
        let s = shared
        let peer = 1 - side
        enum Outcome { case closed, buffered, wake(CheckedContinuation<Data?, Error>) }
        let outcome: Outcome = s.lock.withLock {
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
        let s = shared
        s.lock.lock()
        if s.closed {
            s.lock.unlock()
            return
        }
        s.closed = true
        let ws = s.waiters
        s.waiters = [nil, nil]
        // Bytes already sent stay readable for the other side, as on a socket;
        // this side drops what it had not read.
        s.buffers[side].removeAll()
        s.lock.unlock()
        for w in ws { w?.resume(returning: nil) }
        s.onClose()
    }
}
