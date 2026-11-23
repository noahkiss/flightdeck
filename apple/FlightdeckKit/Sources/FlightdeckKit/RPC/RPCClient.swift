import Foundation
import HabAPI

/// A JSON-RPC error from the hab. Clients branch on `kind`, never on `message`
/// (hab-logic-api.md 3.6).
public struct RPCError: Error, Sendable, CustomStringConvertible {
    public var code: Int
    public var message: String
    public var data: ErrorData?
    /// The failed call's trace (diagnostics.md 5.2): the one the hab answered, else the one the
    /// app sent. `gff-hab logs --trace` finds the call's lines by it.
    public var trace: String? = nil

    public var kind: ErrorKind? { data?.kind }
    public var description: String {
        let s = data.map { "\($0.kind.rawValue): \($0.hint)" } ?? "\(code): \(message)"
        guard let t = trace else { return s }
        return s + " (trace \(t))"
    }
}

/// The stream ended. `detail` says what the carrier knows, such as ssh's stderr.
public struct ConnectionClosed: Error, Sendable, CustomStringConvertible {
    public var detail: String?
    public var description: String {
        detail.map { "connection closed: \($0)" } ?? "connection closed"
    }
}

/// A call got no response within its time limit.
public struct CallTimedOut: Error, Sendable, CustomStringConvertible {
    public var method: String
    public var description: String { "\(method) timed out" }
}

public struct ProtocolError: Error, Sendable, CustomStringConvertible {
    public var description: String
    public init(_ description: String) { self.description = description }
}

/// A notification from the hab, kept as its raw line so the receiver decodes
/// only the params it wants.
public struct Inbound: Sendable {
    public var method: String
    public var line: Data

    /// Decodes the notification's params.
    public func params<T: Decodable>(_ type: T.Type) throws -> T {
        try JSONDecoder().decode(Envelope<T>.self, from: line).params
    }

    private struct Envelope<T: Decodable>: Decodable { var params: T }
}

/// Splits a byte stream into lines (hab-logic-api.md 3.1: one JSON message per line).
struct LineBuffer {
    static let maxLine = 64 << 20
    private var pending = Data()

    mutating func append(_ chunk: Data) throws -> [Data] {
        pending.append(chunk)
        var lines: [Data] = []
        var start = pending.startIndex
        while let nl = pending[start...].firstIndex(of: 0x0A) {
            let line = pending[start..<nl]
            if !line.isEmpty { lines.append(Data(line)) }
            start = pending.index(after: nl)
        }
        pending = Data(pending[start...])
        if pending.count > Self.maxLine {
            throw ProtocolError("a message is longer than \(Self.maxLine) bytes")
        }
        return lines
    }
}

/// A JSON-RPC 2.0 client over one byte stream. Requests may run side by side;
/// responses match by id. Notifications arrive in wire order on `inbound`.
public actor RPCClient {
    private let stream: ByteStream
    private var nextID = 1
    private var pending: [Int: CheckedContinuation<Data, Error>] = [:]
    private var timeouts: [Int: Task<Void, Never>] = [:]
    private var ended: Error?
    private var started = false
    private let encoder = JSONEncoder()
    private let decoder = JSONDecoder()
    private static let log = AppLog(category: "rpc")

    /// When the last bytes arrived, and when the last request went out.
    public private(set) var lastReceived = ContinuousClock.now
    public private(set) var lastSent = ContinuousClock.now

    public nonisolated let inbound: AsyncStream<Inbound>
    private let inboundSink: AsyncStream<Inbound>.Continuation

    public init(stream: ByteStream) {
        self.stream = stream
        (inbound, inboundSink) = AsyncStream.makeStream(of: Inbound.self)
    }

    /// Starts reading. Call once, before the first request.
    public func start() {
        guard !started else { return }
        started = true
        Task { await self.readLoop() }
    }

    /// Sends a request and waits for its response.
    public func call<P, R>(_ method: HabMethod<P, R>, _ params: P, timeout: Duration = .seconds(30)) async throws -> R {
        if let e = ended { throw e }
        let id = nextID
        nextID += 1
        // Each call is one user action, so it gets a new trace (diagnostics.md 5.2).
        let sent = Trace.new()
        var data = try encoder.encode(Request(id: id, trace: sent, method: method.name, params: params))
        data.append(0x0A)
        let line: Data = try await withCheckedThrowingContinuation { c in
            pending[id] = c
            Task { await self.send(data, id: id) }
            timeouts[id] = Task {
                try? await Task.sleep(for: timeout)
                if Task.isCancelled { return }
                self.fail(id, CallTimedOut(method: method.name))
            }
        }
        let resp = try decoder.decode(Response<R>.self, from: line)
        if let e = resp.error {
            // The hab's answer, when it has a trace's form; a trace holds no content.
            let tr = resp.trace.flatMap { Trace.isValid($0) ? $0 : nil } ?? sent
            let err = RPCError(code: e.code, message: e.message, data: e.data, trace: tr)
            Self.log.notice(Self.failedCall(method.name, err, trace: tr))
            throw err
        }
        guard let r = resp.result else {
            throw ProtocolError("\(method.name): response has neither result nor error")
        }
        return r
    }

    /// Closes the stream. Waiting calls fail.
    public func close() {
        finish(ConnectionClosed(detail: "closed by the app"))
    }

    public var isClosed: Bool { ended != nil }

    /// Why the stream ended, once it has; nil while open.
    public var endError: Error? { ended }

    private func send(_ data: Data, id: Int) async {
        do {
            lastSent = .now
            try await stream.write(data)
        } catch {
            fail(id, error)
        }
    }

    private func fail(_ id: Int, _ error: Error) {
        timeouts.removeValue(forKey: id)?.cancel()
        pending.removeValue(forKey: id)?.resume(throwing: error)
    }

    private func readLoop() async {
        var lines = LineBuffer()
        do {
            while let chunk = try await stream.read() {
                lastReceived = .now
                for line in try lines.append(chunk) {
                    handle(line)
                }
            }
            finish(ConnectionClosed(detail: stream.diagnostics))
        } catch {
            finish(error)
        }
    }

    private func handle(_ line: Data) {
        let head: Head
        do {
            head = try decoder.decode(Head.self, from: line)
        } catch {
            // The line is dropped. The log names the method when it can, the decode error's kind
            // and the line's size; never the line, which can hold a user's text
            // (diagnostics.md 4.6).
            let method = Self.method(of: line) ?? "unknown"
            Self.log.error("dropped a hab line that does not decode: method \(word: method), \(word: Self.kind(of: error)), \(line.count) bytes")
            return
        }
        if let m = head.method {
            // The hab sends notifications only; a request from it is ignored.
            if head.id == nil { inboundSink.yield(Inbound(method: m, line: line)) }
            return
        }
        if let id = head.id {
            timeouts.removeValue(forKey: id)?.cancel()
            pending.removeValue(forKey: id)?.resume(returning: line)
        }
    }

    private func finish(_ error: Error) {
        guard ended == nil else { return }
        ended = error
        let waiting = pending
        pending.removeAll()
        for (_, t) in timeouts { t.cancel() }
        timeouts.removeAll()
        for (_, c) in waiting { c.resume(throwing: error) }
        inboundSink.finish()
        stream.close()
    }

    private struct Request<P: Encodable>: Encodable {
        var jsonrpc = "2.0"
        var id: Int
        var trace: String
        var method: String
        var params: P
    }

    private struct Head: Decodable {
        var id: Int?
        var method: String?
    }

    /// The method of a line, when it is a JSON object whose method is an API word.
    private static func method(of line: Data) -> String? {
        guard let obj = try? JSONSerialization.jsonObject(with: line) as? [String: Any],
              let m = obj["method"] as? String, m.count <= 64,
              m.allSatisfy({ $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "." || $0 == "_") })
        else { return nil }
        return m
    }

    /// A decode error's kind and the coding path, which holds schema keys only.
    private static func kind(of error: Error) -> String {
        guard let e = error as? DecodingError else { return "not a decoding error" }
        let path: [CodingKey]
        let kind: String
        switch e {
        case .typeMismatch(_, let c): (kind, path) = ("type mismatch", c.codingPath)
        case .valueNotFound(_, let c): (kind, path) = ("value not found", c.codingPath)
        case .keyNotFound(let k, let c): (kind, path) = ("key not found", c.codingPath + [k])
        case .dataCorrupted(let c): (kind, path) = ("data corrupted", c.codingPath)
        @unknown default: (kind, path) = ("other", [])
        }
        let at = path.map { $0.intValue.map(String.init) ?? $0.stringValue }.joined(separator: ".")
        return at.isEmpty ? kind : "\(kind) at \(at)"
    }

    private struct ErrorObject: Decodable {
        var code: Int
        var message: String
        var data: ErrorData?

        enum CodingKeys: String, CodingKey { case code, message, data }

        init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            code = try c.decode(Int.self, forKey: .code)
            message = (try? c.decode(String.self, forKey: .message)) ?? ""
            // A plain JSON-RPC error (parse error, unknown method) may carry no data.
            data = try? c.decode(ErrorData.self, forKey: .data)
        }
    }

    private struct Response<R: Decodable>: Decodable {
        var result: R?
        var error: ErrorObject?
        var trace: String?
    }

    /// The line for a failed call: the method, the error kind and the trace, all public.
    static func failedCall(_ method: String, _ error: RPCError, trace tr: String) -> AppLogMessage {
        "call failed: \(word: method), \(kind: error), trace \(id: tr)"
    }
}
