import Foundation
import HabAPI

/// The pane half of `FakeHab`: data tickets, data connections with frames,
/// `pane.input`, size ownership and redraw (hab-logic-api.md 5 and 6), close
/// enough for tests and demo mode. Output is stored whole, so a snapshot is
/// every byte so far: a stand-in for the screen the real hab's parser builds.
public actor FakePanes {
    private struct PaneState {
        var chunks: [(offset: UInt64, data: Data)] = []
        /// The holder offset after the last byte.
        var end: UInt64 = 0
        /// Resumes from below this offset get a snapshot.
        var kept: UInt64 = 0
        var cols = 80
        var rows = 24
        var owner: String?
        var reason: OwnerReason = .disown
        var desired: [String: (cols: Int, rows: Int)] = [:]
        var debounce: (client: String, task: Task<Void, Never>)?
        var exited: ExitedFrame?
        /// Echo input back as output, as a shell in cooked mode does.
        var echo = false
    }

    /// One data connection. Frames go out through one queue, in the order
    /// they were queued, so a catch-up never races live output.
    private final class DataConn: @unchecked Sendable {
        let stream: ByteStream
        let pane: String
        let client: String
        private let out: AsyncStream<Data>.Continuation
        init(stream: ByteStream, pane: String, client: String) {
            self.stream = stream
            self.pane = pane
            self.client = client
            let (s, c) = AsyncStream.makeStream(of: Data.self)
            out = c
            Task {
                for await d in s {
                    do { try await stream.write(d) } catch { break }
                }
            }
        }
        func queue(_ d: Data) { out.yield(d) }
        func finish() { out.finish() }
    }

    private var panes: [String: PaneState] = [:]
    private var tickets: [String: (pane: String, client: String)] = [:]
    private var conns: [ObjectIdentifier: DataConn] = [:]

    /// Debounce before typing takes ownership (6.2).
    public var typingDebounce: Duration = .milliseconds(300)

    /// Every `pane.input` received: (client, pane, bytes).
    public private(set) var inputs: [(client: String, pane: String, data: Data)] = []
    /// Every desired size received: (client, cols, rows).
    public private(set) var desiredSizes: [(client: String, cols: Int, rows: Int)] = []
    /// Sizes the fake PTY took, oldest first.
    public private(set) var resizes: [(pane: String, cols: Int, rows: Int)] = []
    /// Credit frames received, in bytes.
    public private(set) var creditReturned: UInt64 = 0
    public private(set) var redraws: [RedrawMode] = []
    /// Data hellos received, with their offsets.
    public private(set) var hellos: [UInt64?] = []

    public init() {}

    // MARK: - Panes

    /// Adds a pane with its size. With `echo`, input comes back as output.
    public func add(_ id: String, cols: Int = 80, rows: Int = 24, echo: Bool = false) {
        var p = PaneState()
        p.cols = cols
        p.rows = rows
        p.echo = echo
        panes[id] = p
    }

    public func size(_ id: String) -> (cols: Int, rows: Int)? {
        panes[id].map { ($0.cols, $0.rows) }
    }

    public func owner(_ id: String) -> String? { panes[id]?.owner }

    /// Every byte the pane wrote, in offset order.
    public func output(_ id: String) -> Data {
        panes[id]?.chunks.reduce(into: Data()) { $0.append($1.data) } ?? Data()
    }

    public func end(_ id: String) -> UInt64 { panes[id]?.end ?? 0 }

    /// The program writes `data`: stored, then sent to every stream on the pane.
    public func write(_ id: String, _ data: Data) async {
        guard var p = panes[id], !data.isEmpty else { return }
        let off = p.end
        p.chunks.append((off, data))
        p.end += UInt64(data.count)
        panes[id] = p
        var i = 0
        while i < data.count {
            let n = min(64 << 10, data.count - i)
            let part = data.subdata(in: i..<(i + n))
            send(pane: id, DataFrame.bytes(offset: off + UInt64(i), part))
            i += n
        }
    }

    /// The hab removed `n` bytes (an answered terminal query): the offset
    /// moves on and no client sees them (5.8).
    public func skip(_ id: String, _ n: Int) {
        panes[id]?.end += UInt64(n)
    }

    /// Forgets output below the current end, so an older resume gets a snapshot.
    public func forgetHistory(_ id: String) {
        guard let p = panes[id] else { return }
        panes[id]?.kept = p.end
    }

    /// The program exits: an exited frame on every stream, then each closes.
    public func exit(_ id: String, code: Int) async {
        panes[id]?.exited = ExitedFrame(code: code)
        for c in conns.values where c.pane == id {
            c.queue((try? DataFrame.json(.exited, ExitedFrame(code: code)).encoded()) ?? Data())
            c.finish()
            // The writer drains, then the stream closes.
            let s = c.stream
            Task { try? await Task.sleep(for: .milliseconds(50)); s.close() }
        }
    }

    /// Closes every data connection, as a network drop does.
    public func dropDataConnections() {
        for c in conns.values {
            c.finish()
            c.stream.close()
        }
        conns = [:]
    }

    public var dataConnectionCount: Int { conns.count }

    // MARK: - Control methods (called by FakeHab)

    func ticket(pane: String, client: String) throws -> Ticket {
        guard panes[pane] != nil else { throw FakeFailure(kind: .notFound, hint: "no pane \(pane)") }
        let t = FakeHab.randomID() + FakeHab.randomID()
        tickets[t] = (pane, client)
        return Ticket(expiresInMs: 30_000, seq: 0, ticket: t)
    }

    func input(pane: String, client: String, data: Data) async throws {
        guard var p = panes[pane] else { throw FakeFailure(kind: .notFound, hint: "no pane \(pane)") }
        inputs.append((client, pane, data))
        if p.owner != client, p.debounce?.client != client {
            p.debounce?.task.cancel()
            let wait = typingDebounce
            p.debounce = (client, Task { [weak self] in
                try? await Task.sleep(for: wait)
                guard !Task.isCancelled else { return }
                await self?.typed(pane: pane, client: client)
            })
        } else if p.owner == client {
            p.debounce?.task.cancel()
            p.debounce = nil
        }
        let echo = p.echo
        panes[pane] = p
        if echo {
            var out = Data()
            for b in data {
                switch b {
                case 0x0D: out.append(contentsOf: [0x0D, 0x0A, 0x24, 0x20]) // a new prompt
                case 0x7F: out.append(contentsOf: [0x08, 0x20, 0x08])
                default: out.append(b)
                }
            }
            await write(pane, out)
        }
    }

    func setDesired(pane: String, client: String, cols: Int, rows: Int) async throws -> Bool {
        guard var p = panes[pane] else { throw FakeFailure(kind: .notFound, hint: "no pane \(pane)") }
        desiredSizes.append((client, cols, rows))
        p.desired[client] = (cols, rows)
        panes[pane] = p
        guard p.owner == nil || p.owner == client else { return false }
        await apply(pane: pane, cols: cols, rows: rows)
        return true
    }

    func take(pane: String, client: String) async throws -> String? {
        guard panes[pane] != nil else { throw FakeFailure(kind: .notFound, hint: "no pane \(pane)") }
        await own(pane: pane, client: client, reason: .take)
        return client
    }

    func redraw(pane: String, client: String, mode: RedrawMode) async throws {
        guard let p = panes[pane] else { throw FakeFailure(kind: .notFound, hint: "no pane \(pane)") }
        redraws.append(mode)
        for c in conns.values where c.pane == pane && c.client == client {
            c.queue(snapshotFrame(p).encoded())
            c.queue(resizeFrame(p).encoded())
        }
        if mode == .nudge {
            await apply(pane: pane, cols: max(1, p.cols - 1), rows: p.rows)
            try? await Task.sleep(for: .milliseconds(100))
            await apply(pane: pane, cols: p.cols, rows: p.rows)
        }
    }

    /// A control connection closed: the client's panes keep their size and lose their owner.
    func disconnected(client: String) async {
        for (id, p) in panes where p.owner == client {
            panes[id]?.owner = nil
            panes[id]?.reason = .disconnect
            if let q = panes[id] { send(pane: id, resizeFrame(q)) }
        }
    }

    private func typed(pane: String, client: String) async {
        guard let p = panes[pane], p.debounce?.client == client else { return }
        panes[pane]?.debounce = nil
        await own(pane: pane, client: client, reason: .typed)
    }

    private func own(pane: String, client: String, reason: OwnerReason) async {
        guard var p = panes[pane], p.owner != client else { return }
        p.owner = client
        p.reason = reason
        panes[pane] = p
        send(pane: pane, resizeFrame(p))
        if let d = p.desired[client], d.cols != p.cols || d.rows != p.rows {
            await apply(pane: pane, cols: d.cols, rows: d.rows)
        }
    }

    private func apply(pane: String, cols: Int, rows: Int) async {
        guard var p = panes[pane], p.cols != cols || p.rows != rows else { return }
        p.cols = cols
        p.rows = rows
        panes[pane] = p
        resizes.append((pane, cols, rows))
        send(pane: pane, resizeFrame(p))
    }

    // MARK: - Data connections

    /// Serves one data connection: the hello, then frames until it closes.
    func serve(_ stream: ByteStream) async {
        var buf = FrameBuffer()
        var conn: DataConn?
        defer {
            stream.close()
            if let c = conn {
                c.finish()
                conns[ObjectIdentifier(c)] = nil
            }
        }
        while let chunk = try? await stream.read() {
            guard let frames = try? buf.append(chunk) else { return }
            for f in frames {
                if conn == nil {
                    guard f.kind == FrameKind.hello.rawValue, let h = try? f.decode(DataHello.self) else { return }
                    hellos.append(h.offset)
                    guard let t = tickets.removeValue(forKey: h.ticket) else {
                        try? await stream.write(DataFrame.json(.reject, DataReject(hint: "unknown ticket", kind: .ticketInvalid)).encoded())
                        return
                    }
                    guard let p = panes[t.pane], p.exited == nil else {
                        try? await stream.write(DataFrame.json(.reject, DataReject(hint: "pane gone", kind: .notFound)).encoded())
                        return
                    }
                    // Welcome, catch-up and registration happen with no suspension
                    // between them, so no output is missed or sent twice.
                    let c = DataConn(stream: stream, pane: t.pane, client: t.client)
                    c.queue((try? DataFrame.json(.welcome, DataWelcome(cols: p.cols, owner: p.owner, rows: p.rows, stream: "s" + FakeHab.randomID())).encoded()) ?? Data())
                    c.queue(catchUp(p, from: h.offset))
                    conns[ObjectIdentifier(c)] = c
                    conn = c
                } else if f.kind == FrameKind.credit.rawValue, let cr = try? f.decode(CreditFrame.self) {
                    creditReturned += cr.bytes
                }
            }
        }
    }

    /// Bytes from `offset` when they are kept, else a snapshot.
    private func catchUp(_ p: PaneState, from offset: UInt64?) -> Data {
        guard let off = offset, off >= p.kept, off <= p.end else {
            return snapshotFrame(p).encoded()
        }
        var out = Data()
        for ch in p.chunks where ch.offset + UInt64(ch.data.count) > off {
            let cut = off > ch.offset ? Int(off - ch.offset) : 0
            out.append(DataFrame.bytes(offset: ch.offset + UInt64(cut), ch.data.subdata(in: cut..<ch.data.count)).encoded())
        }
        return out
    }

    private func snapshotFrame(_ p: PaneState) -> DataFrame {
        DataFrame.snapshot(offset: p.end, p.chunks.reduce(into: Data()) { $0.append($1.data) })
    }

    private func resizeFrame(_ p: PaneState) -> DataFrame {
        (try? DataFrame.json(.resize, ResizeFrame(cols: p.cols, owner: p.owner, reason: p.reason, rows: p.rows)))
            ?? DataFrame(kind: FrameKind.resize.rawValue, payload: Data())
    }

    private func send(pane: String, _ f: DataFrame) {
        let d = f.encoded()
        for c in conns.values where c.pane == pane {
            c.queue(d)
        }
    }
}

/// An API error the fake answers with.
struct FakeFailure: Error {
    var kind: ErrorKind
    var hint: String
}
