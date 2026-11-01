import Foundation
import HabAPI

/// Sends a pane's input over the control connection (`pane.input`, hab-logic-api.md 3.8),
/// in order. One call is in flight at a time; bytes typed meanwhile go out
/// together in the next call, so fast typing over a slow link costs one round
/// trip per batch, never a reordering.
public final class PaneInputPump: @unchecked Sendable {
    public typealias Send = @Sendable (Data) async throws -> Void

    private let lock = NSLock()
    private var pending = Data()
    private var running = false
    private let send: Send
    private let onError: @Sendable (Error) -> Void

    public init(send: @escaping Send, onError: @escaping @Sendable (Error) -> Void = { _ in }) {
        self.send = send
        self.onError = onError
    }

    public convenience init(pane: String, connection: HabConnection, onError: @escaping @Sendable (Error) -> Void = { _ in }) {
        self.init(send: { data in
            _ = try await connection.call(HabMethods.paneInput, InputParams(data: data, pane: pane))
        }, onError: onError)
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
                let b = pending
                pending = Data()
                return b
            }
            guard let batch else { return }
            do {
                try await send(batch)
            } catch {
                // A failed call is not sent again: whether the hab wrote it is unknown.
                onError(error)
            }
        }
    }
}

/// Removes the terminal's own replies from what it writes, so only keys reach
/// the pane (hab-logic-api.md 5.6). The hab answers and removes every query it
/// knows; libghostty still answers the few it leaves (OSC 52 clipboard reads,
/// CSI 14 t and CSI 16 t), and those replies must not reach the program.
/// Focus reports are dropped unless this client owns the pane's size.
enum TerminalReplyFilter {
    static func keep(_ data: Data, owner: Bool) -> Data {
        guard data.first == 0x1B, data.count >= 3 else { return data }
        let b = [UInt8](data)
        if b[1] == 0x5D {
            // OSC 52 ; ... : a clipboard reply.
            if b.starts(with: Array("\u{1B}]52;".utf8)) { return Data() }
            return data
        }
        guard b[1] == 0x5B else { return data }
        // Focus in and out: CSI I, CSI O.
        if b.count == 3 && (b[2] == 0x49 || b[2] == 0x4F) { return owner ? data : Data() }
        // Pixel size reports: CSI 4 ; h ; w t and CSI 6 ; h ; w t.
        if b.last == 0x74, b[2] == 0x34 || b[2] == 0x36, b.count > 4, b[3] == 0x3B,
           b[4..<(b.count - 1)].allSatisfy({ ($0 >= 0x30 && $0 <= 0x39) || $0 == 0x3B }) {
            return Data()
        }
        return data
    }
}
