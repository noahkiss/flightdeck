import Foundation

/// What a stream will carry. The hab tells them apart by the first byte
/// (hab-logic-api.md 4.1), so a carrier opens both the same way today. The kind
/// is passed so a carrier can treat them differently later: an SSH carrier
/// opens each as its own channel on one connection (4.2, option A).
public enum StreamKind: Sendable, Hashable {
    /// The control connection: JSON-RPC, one message per line.
    case control
    /// One data stream for one pane: frames (hab-logic-api.md 5).
    case data
}

/// One bidirectional byte stream to a hab.
///
/// A stream has one reader at a time. `close()` ends both directions; after it,
/// `read()` returns nil and `write(_:)` throws.
public protocol ByteStream: AnyObject, Sendable {
    /// The next chunk of bytes, or nil at the end of the stream.
    func read() async throws -> Data?
    /// Writes every byte, or throws.
    func write(_ data: Data) async throws
    /// Closes both directions. Safe to call more than once.
    func close()
    /// Why the stream ended, when the carrier knows more than "closed": for
    /// example the last lines ssh printed on stderr.
    var diagnostics: String? { get }
}

extension ByteStream {
    public var diagnostics: String? { nil }
}

/// A way to reach one hab: the local socket, an ssh subprocess, later the
/// iOS SSH library, or a fake. Each `open` returns a new stream.
public protocol Transport: Sendable {
    /// A short description for the connections list and logs, such as
    /// `local` or `ssh buildbox`.
    var endpoint: String { get }
    /// Opens one stream of the given kind.
    func open(_ kind: StreamKind) async throws -> ByteStream
    /// Drops any shared connection, so the next `open` starts afresh. A carrier
    /// with nothing shared does nothing.
    func reset() async
}

extension Transport {
    public func reset() async {}
}

public struct TransportError: Error, CustomStringConvertible, Sendable {
    public var description: String
    public init(_ description: String) { self.description = description }
}

/// Counts the streams of real carriers (socket and pipe) that are still open.
/// A stream counts from open until its file descriptors are closed. The
/// lifecycle check reads it: no stream may stay open in the background.
public enum OpenStreams {
    private static let lock = NSLock()
    nonisolated(unsafe) private static var count = 0

    /// Streams open now.
    public static var live: Int {
        lock.lock(); defer { lock.unlock() }
        return count
    }

    static func opened() {
        lock.lock(); count += 1; lock.unlock()
    }

    static func closed() {
        lock.lock(); count -= 1; lock.unlock()
    }
}
