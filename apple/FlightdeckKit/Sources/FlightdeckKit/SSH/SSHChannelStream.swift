import Foundation
import NIOCore
import NIOSSH

/// One SSH session channel as a byte stream: stdin and stdout of the remote
/// command (`gff-hab connect`).
///
/// The channel does not read on its own: each `read()` asks for more, so the
/// SSH window opens only as fast as the reader takes bytes. That gives every
/// stream its own flow control (transport.md 4.4).
final class SSHChannelStream: ByteStream, @unchecked Sendable {
    private let lock = NSLock()
    private var chunks: [Data] = []
    private var finished = false
    private var failure: Error?
    private var waiter: CheckedContinuation<Data?, Error>?
    private var channel: Channel?
    private var closing = false
    private var counted = false
    private var ended = false
    private var stderr = Data()
    private var exitStatus: Int?
    private let onEnd: @Sendable () -> Void
    private let onFirstData: (@Sendable () -> Void)?
    private var sawData = false

    init(onEnd: @escaping @Sendable () -> Void, onFirstData: (@Sendable () -> Void)? = nil) {
        self.onEnd = onEnd
        self.onFirstData = onFirstData
    }

    /// The handler to put in the child channel's pipeline.
    func makeHandler(execReply: EventLoopPromise<Void>) -> ChannelHandler {
        Handler(stream: self, execReply: execReply)
    }

    /// Called once the remote command runs. Counts the stream as open.
    func attach(_ ch: Channel) {
        lock.withLock {
            channel = ch
            if !ended {
                counted = true
                OpenStreams.opened()
            }
        }
    }

    var diagnostics: String? {
        lock.withLock {
            var parts: [String] = []
            let s = String(decoding: stderr, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            if !s.isEmpty { parts.append(s) }
            if let e = exitStatus { parts.append("remote command exited with status \(e)") }
            return parts.isEmpty ? nil : parts.joined(separator: "; ")
        }
    }

    func read() async throws -> Data? {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Data?, Error>) in
            lock.lock()
            if !chunks.isEmpty {
                let d = chunks.removeFirst()
                lock.unlock()
                c.resume(returning: d)
            } else if finished {
                let f = failure
                lock.unlock()
                if let f { c.resume(throwing: f) } else { c.resume(returning: nil) }
            } else {
                waiter = c
                let ch = channel
                lock.unlock()
                ch?.read()
            }
        }
    }

    func write(_ data: Data) async throws {
        let ch: Channel? = lock.withLock { closing || finished ? nil : channel }
        guard let ch else { throw TransportError("stream closed") }
        let buf = ch.allocator.buffer(bytes: data)
        do {
            try await ch.writeAndFlush(SSHChannelData(type: .channel, data: .byteBuffer(buf))).get()
        } catch {
            throw TransportError("write: \(error)")
        }
    }

    func close() {
        lock.lock()
        if closing {
            lock.unlock()
            return
        }
        closing = true
        let w = waiter
        waiter = nil
        finished = true
        failure = nil
        chunks.removeAll()
        let ch = channel
        lock.unlock()
        w?.resume(returning: nil)
        ch?.close(promise: nil)
    }

    // MARK: - From the handler (on the event loop)

    fileprivate func deliver(_ d: Data) {
        lock.lock()
        let first = !sawData
        sawData = true
        if finished {
            lock.unlock()
            return
        }
        if let w = waiter {
            waiter = nil
            lock.unlock()
            w.resume(returning: d)
        } else {
            chunks.append(d)
            lock.unlock()
        }
        if first { onFirstData?() }
    }

    fileprivate func appendStderr(_ d: Data) {
        lock.withLock {
            stderr.append(d)
            if stderr.count > 2048 { stderr = stderr.suffix(2048) }
        }
    }

    fileprivate func setExit(_ status: Int) {
        lock.withLock { exitStatus = status }
    }

    /// End of input (remote EOF) or a failure. Bytes already queued stay readable.
    fileprivate func finish(_ error: Error?) {
        lock.lock()
        if finished {
            lock.unlock()
            return
        }
        finished = true
        failure = error
        let w = chunks.isEmpty ? waiter : nil
        if w != nil { waiter = nil }
        lock.unlock()
        if let w {
            if let error { w.resume(throwing: error) } else { w.resume(returning: nil) }
        }
    }

    /// The channel closed. Runs once.
    fileprivate func channelEnded() {
        finish(nil)
        let wasCounted: Bool = lock.withLock {
            guard !ended else { return false }
            ended = true
            return counted
        }
        if wasCounted { OpenStreams.closed() }
        onEnd()
    }

    private final class Handler: ChannelInboundHandler {
        typealias InboundIn = SSHChannelData

        let stream: SSHChannelStream
        let execReply: EventLoopPromise<Void>
        private var replied = false
        private var gone = false

        init(stream: SSHChannelStream, execReply: EventLoopPromise<Void>) {
            self.stream = stream
            self.execReply = execReply
        }

        func channelRead(context: ChannelHandlerContext, data: NIOAny) {
            let msg = unwrapInboundIn(data)
            guard case .byteBuffer(let buf) = msg.data else { return }
            let d = Data(buf.readableBytesView)
            switch msg.type {
            case .channel: stream.deliver(d)
            case .stdErr: stream.appendStderr(d)
            default: break
            }
        }

        func userInboundEventTriggered(context: ChannelHandlerContext, event: Any) {
            switch event {
            case is ChannelSuccessEvent:
                reply(nil)
            case is ChannelFailureEvent:
                reply(TransportError("the host refused to run the command"))
            case let s as SSHChannelRequestEvent.ExitStatus:
                stream.setExit(s.exitStatus)
            case ChannelEvent.inputClosed:
                stream.finish(nil)
            default:
                context.fireUserInboundEventTriggered(event)
            }
        }

        func errorCaught(context: ChannelHandlerContext, error: Error) {
            reply(error)
            stream.finish(TransportError("ssh channel: \(error)"))
            context.close(promise: nil)
        }

        func handlerRemoved(context: ChannelHandlerContext) {
            ended()
        }

        func channelInactive(context: ChannelHandlerContext) {
            ended()
            context.fireChannelInactive()
        }

        private func ended() {
            guard !gone else { return }
            gone = true
            reply(ConnectionClosed(detail: stream.diagnostics ?? "the ssh channel closed before the command started"))
            stream.channelEnded()
        }

        private func reply(_ error: Error?) {
            guard !replied else { return }
            replied = true
            if let error { execReply.fail(error) } else { execReply.succeed(()) }
        }
    }
}
