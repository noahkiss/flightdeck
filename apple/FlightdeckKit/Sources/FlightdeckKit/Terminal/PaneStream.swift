import Foundation
import HabAPI

/// What a pane stream hands its consumer, in stream order.
public enum PaneStreamEvent: Sendable, Equatable {
    /// A data connection opened: the pane's size and owner now.
    case welcome(DataWelcome)
    /// Replace the screen with these bytes. `offset` is the holder offset they cover.
    case snapshot(offset: UInt64, encoding: String, data: Data)
    /// Program output starting at holder offset `offset`.
    case bytes(offset: UInt64, data: Data)
    case resize(ResizeFrame)
    /// The program exited. The stream ends after this.
    case exited(ExitedFrame)
    /// The data connection dropped or could not open; the stream retries.
    case retrying(String)
    /// The stream ended for good: the pane is gone, the program exited, or `close` ran.
    case ended(PaneStreamEnd)
}

public enum PaneStreamEnd: Sendable, Equatable {
    case exited
    case gone
    case closed
}

/// Timings of a pane stream; tests shorten them.
public struct PaneStreamTiming: Sendable {
    public var minBackoff: Duration = .milliseconds(250)
    public var maxBackoff: Duration = .seconds(5)
    /// Credit goes back once this many bytes were applied (as in the Go library).
    public var creditEvery = 16 << 10
    public init() {}
}

/// One data stream for one pane (hab-logic-api.md 5): a ticket over the control
/// connection, a data connection on the hab's carrier, a hello with the held
/// offset, then frames. After a drop it opens a new connection with a new
/// ticket and the offset after the last byte it applied, so the hab continues
/// with no gap and no repeat, or sends a snapshot.
///
/// The consumer's `apply` is awaited for each event; credit is returned only
/// for bytes `apply` has taken, so a slow consumer slows the hab, not memory.
public actor PaneStream {
    public typealias Ticket = @Sendable (_ observe: Bool) async throws -> String
    public typealias Open = @Sendable () async throws -> ByteStream
    public typealias Apply = @Sendable (PaneStreamEvent) async -> Void

    public nonisolated let pane: String
    private let ticket: Ticket
    private let openStream: Open
    private let observe: Bool
    private let timing: PaneStreamTiming
    private var apply: Apply = { _ in }

    /// The holder offset after the last byte applied; nil before the first frame.
    public private(set) var offset: UInt64?
    /// Data connections opened (welcomed), over the stream's life.
    public private(set) var connections = 0
    /// Snapshots applied.
    public private(set) var snapshots = 0
    /// Bytes trimmed because a frame started below the held offset. The hab
    /// never sends one (5.4); tests assert this stays zero.
    public private(set) var trimmed = 0
    /// Hellos sent, with the offset each carried.
    public private(set) var helloOffsets: [UInt64?] = []

    private var task: Task<Void, Never>?
    private var current: ByteStream?
    private var closed = false

    public init(pane: String, observe: Bool = false, timing: PaneStreamTiming = PaneStreamTiming(), ticket: @escaping Ticket, open: @escaping Open) {
        self.pane = pane
        self.observe = observe
        self.timing = timing
        self.ticket = ticket
        self.openStream = open
    }

    /// A stream over a hab connection: tickets from its control connection,
    /// data connections on its transport.
    public init(pane: String, connection: HabConnection, observe: Bool = false, timing: PaneStreamTiming = PaneStreamTiming()) {
        let t = connection.transport
        self.init(pane: pane, observe: observe, timing: timing, ticket: { obs in
            try await connection.call(HabMethods.dataTicket, TicketParams(observe: obs ? true : nil, pane: pane)).ticket
        }, open: {
            try await t.open(.data)
        })
    }

    /// Starts the stream. `from` is an offset kept from an earlier stream on
    /// this pane, to resume it.
    public func start(from: UInt64? = nil, apply: @escaping Apply) {
        guard task == nil, !closed else { return }
        self.apply = apply
        offset = from
        task = Task { await self.run() }
    }

    /// Ends the stream and closes its data connection. Returns at once.
    public func close() {
        guard !closed else { return }
        closed = true
        task?.cancel()
        current?.close()
        current = nil
    }

    /// Drops the current data connection, as a network loss would. The stream
    /// resumes on a new one. For tests and for a control reconnect.
    public func dropConnection() {
        current?.close()
    }

    // MARK: - Loop

    private struct Retry: Error, CustomStringConvertible {
        var description: String
    }

    private struct End: Error {
        var reason: PaneStreamEnd
    }

    private func run() async {
        var failures = 0
        var end = PaneStreamEnd.closed
        while !closed && !Task.isCancelled {
            if failures > 0 {
                try? await Task.sleep(for: HabConnection.backoff(attempt: failures, lo: timing.minBackoff, hi: timing.maxBackoff))
                if closed || Task.isCancelled { break }
            }
            do {
                try await session(onWelcome: { failures = 0 })
                // The connection ended with no exit: resume.
                failures = max(failures, 0) + 1
                if !closed { await apply(.retrying("data connection closed")) }
            } catch let e as End {
                end = e.reason
                break
            } catch let e as RPCError where e.kind == .notFound {
                end = .gone
                break
            } catch {
                failures += 1
                if !closed { await apply(.retrying(String(describing: error))) }
            }
        }
        current?.close()
        current = nil
        await apply(.ended(closed ? .closed : end))
    }

    private func session(onWelcome: () -> Void) async throws {
        let t = try await ticket(observe)
        let s = try await openStream()
        if closed {
            s.close()
            throw End(reason: .closed)
        }
        current = s
        defer {
            s.close()
            if current === s { current = nil }
        }
        helloOffsets.append(offset)
        try await s.write(DataFrame.json(.hello, DataHello(offset: offset, ticket: t)).encoded())

        var buf = FrameBuffer()
        var welcomed = false
        var owed = 0
        while let chunk = try await s.read() {
            for f in try buf.append(chunk) {
                guard let kind = FrameKind(rawValue: f.kind) else { continue }
                if !welcomed {
                    switch kind {
                    case .welcome:
                        welcomed = true
                        connections += 1
                        onWelcome()
                        await apply(.welcome(try f.decode(DataWelcome.self)))
                        continue
                    case .reject:
                        try reject(f)
                    default:
                        throw ProtocolError("first data frame is \(kind), not welcome")
                    }
                }
                switch kind {
                case .snapshot:
                    let p = try f.offsetPayload(named: true)
                    await apply(.snapshot(offset: p.offset, encoding: p.encoding, data: p.data))
                    offset = p.offset
                    snapshots += 1
                    owed += p.data.count
                case .bytes:
                    let p = try f.offsetPayload(named: false)
                    var start = p.offset
                    var data = p.data
                    owed += data.count
                    if let held = offset, start < held {
                        // Guard only: drop bytes already applied.
                        let cut = Int(min(UInt64(data.count), held - start))
                        trimmed += cut
                        data = Data(data.dropFirst(cut))
                        start += UInt64(cut)
                    }
                    // A start above the held offset is a removed terminal query (5.8).
                    if !data.isEmpty {
                        await apply(.bytes(offset: start, data: data))
                    }
                    offset = max(offset ?? 0, start + UInt64(data.count))
                case .resize:
                    await apply(.resize(try f.decode(ResizeFrame.self)))
                case .exited:
                    await apply(.exited(try f.decode(ExitedFrame.self)))
                    throw End(reason: .exited)
                case .reject:
                    try reject(f)
                default:
                    break
                }
                if owed >= timing.creditEvery {
                    try await s.write(DataFrame.json(.credit, CreditFrame(bytes: UInt64(owed))).encoded())
                    owed = 0
                }
            }
        }
    }

    private func reject(_ f: DataFrame) throws -> Never {
        let r = try f.decode(DataReject.self)
        if r.kind == .notFound { throw End(reason: .gone) }
        throw Retry(description: "data connection rejected: \(r.kind.rawValue): \(r.hint)")
    }
}
