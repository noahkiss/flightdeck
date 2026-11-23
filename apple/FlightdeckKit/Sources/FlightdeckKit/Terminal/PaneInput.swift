import Foundation
import HabAPI
import Synchronization

/// Input the pump did not send, because a `pane.input` call failed.
public struct InputDropped: Error, CustomStringConvertible, Sendable {
    /// Bytes queued behind the failed call, dropped with it.
    public var dropped: Int
    /// Why the call failed.
    public var cause: String
    /// The call failed because the connection ended or the call timed out, not because
    /// the hab refused it.
    public var connectionLost: Bool
    public var description: String { "input not sent (\(dropped) more bytes dropped): \(cause)" }

    static func isConnectionLoss(_ error: Error) -> Bool {
        error is ConnectionClosed || error is CallTimedOut || error is TransportError
    }
}

/// Sends a pane's input over the control connection (`pane.input`, hab-logic-api.md 3.8),
/// in order. One call is in flight at a time; bytes typed meanwhile go out
/// together in the next call, so fast typing over a slow link costs one round
/// trip per batch, never a reordering.
///
/// A batch is at most 64 KiB (app.md 5.3). Each reply is bytes from the hab, so a
/// large paste over a slow uplink keeps the connection's liveness check fed and
/// each call inside its timeout, and pings go out between batches. Calls stay one at
/// a time: RPCClient writes each request from its own task, so two calls in flight
/// could reach the hab in either order.
///
/// When a call fails, the input stops at the failed call: nothing queued behind it is
/// sent, and the pump reports once. One exception: when the pane may be left inside a
/// bracketed paste (the hab took the failed call or not), an end marker is sent next, so
/// the program does not stay in paste mode.
public final class PaneInputPump: Sendable {
    public typealias Send = @Sendable (Data) async throws -> Void
    public typealias OnError = @Sendable (InputDropped) -> Void

    private struct State {
        var pending = Data()
        var running = false
        /// True while `pending` starts with an end marker the pump owes the pane.
        var owesEnd = false
        /// Whether the bytes the hab took so far leave the pane inside a bracketed paste, and
        /// their last few bytes, so a marker cut by a batch boundary is still seen.
        var inPaste = false
        var carry = Data()
        var onError: OnError
    }

    private let state: Mutex<State>
    private let send: Send
    /// Bytes per `pane.input` call at most.
    static let maxBatch = 64 << 10
    /// The start of a bracketed paste.
    static let pasteStart = Data("\u{1b}[200~".utf8)
    /// The end of a bracketed paste.
    static let pasteEnd = Data("\u{1b}[201~".utf8)

    public init(send: @escaping Send, onError: @escaping OnError = { _ in }) {
        self.send = send
        state = Mutex(State(onError: onError))
    }

    public convenience init(pane: String, connection: HabConnection, onError: @escaping OnError = { _ in }) {
        self.init(send: { data in
            _ = try await connection.call(HabMethods.paneInput, InputParams(data: data, pane: pane))
        }, onError: onError)
    }

    /// Replaces the error handler, including one passed to `init`: the terminal model
    /// sets its own once it exists, over the one its `Calls` pump came with.
    public func setOnError(_ f: @escaping OnError) {
        state.withLock { $0.onError = f }
    }

    /// Queues bytes. Safe from any thread.
    public func enqueue(_ data: Data) {
        guard !data.isEmpty else { return }
        let start: Bool = state.withLock { s in
            s.pending.append(data)
            if s.running { return false }
            s.running = true
            return true
        }
        if start { Task.detached { await self.drain() } }
    }

    private func drain() async {
        while true {
            let next: (batch: Data, owed: Bool)? = state.withLock { s in
                if s.pending.isEmpty {
                    s.running = false
                    return nil
                }
                let owed = s.owesEnd
                s.owesEnd = false
                // An owed end marker goes alone, so a failure can tell it from typed bytes.
                let n = owed ? Self.pasteEnd.count : min(s.pending.count, Self.maxBatch)
                let b = s.pending.prefix(n)
                s.pending = Data(s.pending.dropFirst(n))
                return (Data(b), owed)
            }
            guard let next else { return }
            do {
                try await send(next.batch)
                state.withLock { s in
                    let seen = s.carry + next.batch
                    s.inPaste = Self.inPaste(after: seen, from: s.inPaste)
                    s.carry = Data(seen.suffix(Self.pasteEnd.count - 1))
                }
            } catch {
                // A failed call is not sent again: whether the hab wrote it is unknown.
                // What waits behind it goes too, so nothing lands after a gap.
                let (dropped, stop, report): (Int, Bool, OnError) = state.withLock { s in
                    // The hab took the failed call whole or not at all. If either way leaves
                    // the pane inside a paste, the pump owes it an end marker.
                    let after = Self.inPaste(after: s.carry + next.batch, from: s.inPaste)
                    let endLost = next.owed || s.inPaste || after
                    s.inPaste = false
                    s.carry = Data()
                    let n = s.pending.count
                    s.pending = Data()
                    if endLost {
                        s.pending = Self.pasteEnd
                        s.owesEnd = true
                    }
                    // The owed marker failed again: hold it until the next input,
                    // rather than spin on a connection that is down.
                    let stop = next.owed
                    if stop { s.running = false }
                    return (n, stop, s.onError)
                }
                if dropped > 0 || !next.owed {
                    report(InputDropped(dropped: dropped, cause: String(describing: error),
                                        connectionLost: InputDropped.isConnectionLoss(error)))
                }
                if stop { return }
            }
        }
    }

    /// Whether the pane is inside a bracketed paste after `data`, given whether it was
    /// before: the last marker in `data` decides.
    static func inPaste(after data: Data, from before: Bool) -> Bool {
        let start = data.range(of: pasteStart, options: .backwards)?.lowerBound
        let end = data.range(of: pasteEnd, options: .backwards)?.lowerBound
        switch (start, end) {
        case (nil, nil): return before
        case (.some, nil): return true
        case (nil, .some): return false
        case let (s?, e?): return s > e
        }
    }
}

/// Decides what of the terminal's writes reaches the pane (hab-logic-api.md
/// 5.6, 6.2).
///
/// - libghostty answers the queries the hab left in the stream (OSC 52
///   clipboard reads, pixel size reports, a query with a control byte inside or
///   cut by a gap). Those answers must not reach the program, so anything shaped
///   like a reply is dropped.
/// - A cursor position report looks like a modified F3 key (`CSI 1;2R`). It is
///   dropped while this client does not own the pane; the owner's F3 passes.
/// - Focus reports, mouse motion and the mouse wheel pass only while this
///   client owns the pane: any input from a client that does not own the pane
///   takes the size after the debounce, and hovering or scrolling a viewed
///   pane must not. Clicks pass: a click is meant as input.
/// - In a pane that runs a round's live run (`liveRun`), focus reports, mouse
///   motion and the wheel never pass, owner or not: the hab ends a run taken
///   over on any client input (rounds.md 13), and focusing or scrolling is not
///   meant as input (app.md 21.4).
enum TerminalReplyFilter {
    static func keep(_ data: Data, owner: Bool, liveRun: Bool = false) -> Data {
        guard data.first == 0x1B, data.count >= 3 else { return data }
        let b = [UInt8](data)
        switch b[1] {
        case 0x5D: return osc(b) ? Data() : data
        case 0x50: return dcsReply(b) ? Data() : data
        case 0x5B: return csiKeep(b, owner: owner, pointer: owner && !liveRun) ? data : Data()
        default: return data
        }
    }

    /// OSC replies: a clipboard read, and colour answers (OSC 4, 10, 11, 12 with `rgb:`).
    private static func osc(_ b: [UInt8]) -> Bool {
        let s = String(decoding: b.dropFirst(2), as: UTF8.self)
        if s.hasPrefix("52;") { return true }
        for p in ["4;", "10;", "11;", "12;"] where s.hasPrefix(p) && s.contains("rgb:") {
            return true
        }
        return false
    }

    /// DCS replies (DECRQSS `P1$r`, XTGETTCAP `P1+r`, XTVERSION `P>|`). Alt+Shift+P
    /// alone is `ESC P` and passes.
    private static func dcsReply(_ b: [UInt8]) -> Bool {
        guard b.count >= 4 else { return false }
        let c = b[2]
        return (c >= 0x30 && c <= 0x39) || c == 0x3E
    }

    /// `pointer`: focus reports, mouse motion and the wheel may pass.
    private static func csiKeep(_ b: [UInt8], owner: Bool, pointer: Bool) -> Bool {
        guard let final = b.last else { return true }
        let body = String(decoding: b[2..<(b.count - 1)], as: UTF8.self)
        // Focus in and out.
        if b.count == 3 && (final == 0x49 || final == 0x4F) { return pointer }
        // Legacy mouse: CSI M, then three bytes.
        if b.count == 6 && b[2] == 0x4D {
            return pointer || !motionOrWheel(Int(b[3]) - 32)
        }
        // SGR mouse: CSI < b ; x ; y M or m.
        if body.hasPrefix("<"), final == 0x4D || final == 0x6D {
            let code = Int(body.dropFirst().split(separator: ";").first ?? "") ?? 0
            return pointer || !motionOrWheel(code)
        }
        let digits = body.allSatisfy { $0.isNumber || $0 == ";" }
        switch final {
        case 0x63: // c: device attributes replies, CSI ? ... c and CSI > ... c
            if body.hasPrefix("?") || body.hasPrefix(">") { return false }
        case 0x79: // y: mode reports, CSI ? ... $ y and CSI ... $ y
            if body.hasSuffix("$") { return false }
        case 0x75: // u: keyboard protocol flags, CSI ? flags u
            if body.hasPrefix("?") { return false }
        case 0x6E: // n: device status, CSI 0 n
            if body == "0" || body == "3" { return false }
        case 0x52: // R: cursor position, CSI row ; col R (also a modified F3)
            if digits, body.contains(";"), !owner { return false }
            if body.hasPrefix("?") { return false }
        case 0x74: // t: size reports, CSI 4|6|8 ; a ; b t
            let parts = body.split(separator: ";")
            if digits, parts.count == 3, ["4", "6", "8"].contains(String(parts[0])) { return false }
        default:
            break
        }
        return true
    }

    /// Mouse button code: motion sets 32, the wheel sets 64.
    private static func motionOrWheel(_ code: Int) -> Bool {
        code & 32 != 0 || code & 64 != 0
    }
}
