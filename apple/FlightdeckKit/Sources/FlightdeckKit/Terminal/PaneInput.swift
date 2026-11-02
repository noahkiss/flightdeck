import Foundation
import HabAPI

/// Input the pump did not send, because a `pane.input` call failed.
public struct InputDropped: Error, CustomStringConvertible, Sendable {
    /// Bytes queued behind the failed call, dropped with it.
    public var dropped: Int
    /// Why the call failed.
    public var cause: String
    public var description: String { "input not sent (\(dropped) more bytes dropped): \(cause)" }
}

/// Sends a pane's input over the control connection (`pane.input`, hab-logic-api.md 3.8),
/// in order. One call is in flight at a time; bytes typed meanwhile go out
/// together in the next call, so fast typing over a slow link costs one round
/// trip per batch, never a reordering.
///
/// A batch is at most 64 KiB (app.md 5.3). Each reply is bytes from the hab, so a
/// large paste over a slow uplink keeps the connection's liveness check fed and
/// each call inside its timeout, and pings go out between batches.
///
/// When a call fails, the pump drops everything still queued and reports once: sending
/// the rest after a reconnect would leave a hole in the middle of the input.
public final class PaneInputPump: @unchecked Sendable {
    public typealias Send = @Sendable (Data) async throws -> Void
    public typealias OnError = @Sendable (InputDropped) -> Void

    private let lock = NSLock()
    private var pending = Data()
    private var running = false
    private let send: Send
    private var onError: OnError
    /// Bytes per `pane.input` call at most.
    static let maxBatch = 64 << 10

    public init(send: @escaping Send, onError: @escaping OnError = { _ in }) {
        self.send = send
        self.onError = onError
    }

    public convenience init(pane: String, connection: HabConnection, onError: @escaping OnError = { _ in }) {
        self.init(send: { data in
            _ = try await connection.call(HabMethods.paneInput, InputParams(data: data, pane: pane))
        }, onError: onError)
    }

    /// Replaces the error handler; the terminal model sets it once it exists.
    public func setOnError(_ f: @escaping OnError) {
        lock.withLock { onError = f }
    }

    /// Queues bytes. Safe from any thread.
    public func enqueue(_ data: Data) {
        guard !data.isEmpty else { return }
        let start: Bool = lock.withLock {
            pending.append(data)
            if running { return false }
            running = true
            return true
        }
        if start { Task.detached { await self.drain() } }
    }

    private func drain() async {
        while true {
            let batch: Data? = lock.withLock {
                if pending.isEmpty {
                    running = false
                    return nil
                }
                let n = min(pending.count, Self.maxBatch)
                let b = pending.prefix(n)
                pending = Data(pending.dropFirst(n))
                return Data(b)
            }
            guard let batch else { return }
            do {
                try await send(batch)
            } catch {
                // A failed call is not sent again: whether the hab wrote it is unknown.
                // What waits behind it goes too, so the pane never gets input with a gap.
                let (dropped, report): (Int, OnError) = lock.withLock {
                    let n = pending.count
                    pending = Data()
                    return (n, onError)
                }
                report(InputDropped(dropped: dropped, cause: String(describing: error)))
            }
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
enum TerminalReplyFilter {
    static func keep(_ data: Data, owner: Bool) -> Data {
        guard data.first == 0x1B, data.count >= 3 else { return data }
        let b = [UInt8](data)
        switch b[1] {
        case 0x5D: return osc(b) ? Data() : data
        case 0x50: return dcsReply(b) ? Data() : data
        case 0x5B: return csiKeep(b, owner: owner) ? data : Data()
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

    private static func csiKeep(_ b: [UInt8], owner: Bool) -> Bool {
        guard let final = b.last else { return true }
        let body = String(decoding: b[2..<(b.count - 1)], as: UTF8.self)
        // Focus in and out.
        if b.count == 3 && (final == 0x49 || final == 0x4F) { return owner }
        // Legacy mouse: CSI M, then three bytes.
        if b.count == 6 && b[2] == 0x4D {
            return owner || !motionOrWheel(Int(b[3]) - 32)
        }
        // SGR mouse: CSI < b ; x ; y M or m.
        if body.hasPrefix("<"), final == 0x4D || final == 0x6D {
            let code = Int(body.dropFirst().split(separator: ";").first ?? "") ?? 0
            return owner || !motionOrWheel(code)
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
