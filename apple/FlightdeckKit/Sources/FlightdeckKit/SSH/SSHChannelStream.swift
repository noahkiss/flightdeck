import Foundation
import NIOCore
import NIOSSH
import Synchronization

/// One SSH session channel as a byte stream: stdin and stdout of the remote
/// command (`gff-hab connect`).
///
/// The channel does not read on its own: each `read()` asks for more, so the
/// SSH window opens only as fast as the reader takes bytes. That gives every
/// stream its own flow control (transport.md 4.4).
final class SSHChannelStream: ByteStream, Sendable {
    private struct State {
        var chunks: [Data] = []
        var finished = false
        var failure: Error?
        var waiter: CheckedContinuation<Data?, Error>?
        var channel: Channel?
        var closing = false
        var counted = false
        var ended = false
        var stderr = Data()
        var exitStatus: Int?
        var sawData = false
    }

    private let state = Mutex(State())
    private let onEnd: @Sendable () -> Void
    private let onFirstData: (@Sendable () -> Void)?

    init(onEnd: @escaping @Sendable () -> Void, onFirstData: (@Sendable () -> Void)? = nil) {
        self.onEnd = onEnd
        self.onFirstData = onFirstData
    }

    /// The handler to put in the child channel's pipeline. Call it on the
    /// child's event loop: the handler stays there.
    func makeHandler(execReply: EventLoopPromise<Void>) -> ChannelHandler {
        Handler(stream: self, execReply: execReply)
    }

    /// Called once the remote command runs. Counts the stream as open.
    func attach(_ ch: Channel) {
        state.withLock { s in
            s.channel = ch
            if !s.ended {
                s.counted = true
                OpenStreams.opened()
            }
        }
    }

    var diagnostics: String? {
        state.withLock { s in
            var parts: [String] = []
            let text = String(decoding: s.stderr, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            if !text.isEmpty { parts.append(text) }
            if let e = s.exitStatus { parts.append("remote command exited with status \(e)") }
            return parts.isEmpty ? nil : parts.joined(separator: "; ")
        }
    }

    func read() async throws -> Data? {
        try await withCheckedThrowingContinuation { (c: CheckedContinuation<Data?, Error>) in
            enum Next { case data(Data), end(Error?), wait(Channel?) }
            let next: Next = state.withLock { s in
                if !s.chunks.isEmpty { return .data(s.chunks.removeFirst()) }
                if s.finished { return .end(s.failure) }
                s.waiter = c
                return .wait(s.channel)
            }
            switch next {
            case .data(let d): c.resume(returning: d)
            case .end(let f?): c.resume(throwing: f)
            case .end(nil): c.resume(returning: nil)
            case .wait(let ch): ch?.read()
            }
        }
    }

    func write(_ data: Data) async throws {
        let ch: Channel? = state.withLock { $0.closing || $0.finished ? nil : $0.channel }
        guard let ch else { throw TransportError("stream closed") }
        let buf = ch.allocator.buffer(bytes: data)
        do {
            try await ch.writeAndFlush(SSHChannelData(type: .channel, data: .byteBuffer(buf))).get()
        } catch {
            throw TransportError("write: \(error)")
        }
    }

    /// Ends the remote command's input (SSH EOF); output stays readable.
    func closeWrite() async {
        let ch: Channel? = state.withLock { $0.closing || $0.finished ? nil : $0.channel }
        try? await ch?.close(mode: .output).get()
    }

    /// The remote command's exit status, once the host sent it.
    var exit: Int? { state.withLock { $0.exitStatus } }

    func close() {
        let taken: (waiter: CheckedContinuation<Data?, Error>?, channel: Channel?)? = state.withLock { s in
            if s.closing { return nil }
            s.closing = true
            let w = s.waiter
            s.waiter = nil
            s.finished = true
            s.failure = nil
            s.chunks.removeAll()
            return (w, s.channel)
        }
        guard let taken else { return }
        taken.waiter?.resume(returning: nil)
        taken.channel?.close(promise: nil)
    }

    // MARK: - From the handler (on the event loop)

    fileprivate func deliver(_ d: Data) {
        let (first, w, keep): (Bool, CheckedContinuation<Data?, Error>?, Bool) = state.withLock { s in
            let first = !s.sawData
            s.sawData = true
            if s.finished { return (first, nil, false) }
            if let w = s.waiter {
                s.waiter = nil
                return (first, w, true)
            }
            s.chunks.append(d)
            return (first, nil, true)
        }
        guard keep else { return }
        w?.resume(returning: d)
        if first { onFirstData?() }
    }

    fileprivate func appendStderr(_ d: Data) {
        state.withLock { s in
            s.stderr.append(d)
            if s.stderr.count > 2048 { s.stderr = s.stderr.suffix(2048) }
        }
    }

    fileprivate func setExit(_ status: Int) {
        state.withLock { $0.exitStatus = status }
    }

    /// End of input (remote EOF) or a failure. Bytes already queued stay readable.
    fileprivate func finish(_ error: Error?) {
        let w: CheckedContinuation<Data?, Error>? = state.withLock { s in
            if s.finished { return nil }
            s.finished = true
            s.failure = error
            guard s.chunks.isEmpty else { return nil }
            let w = s.waiter
            s.waiter = nil
            return w
        }
        if let w {
            if let error { w.resume(throwing: error) } else { w.resume(returning: nil) }
        }
    }

    /// The channel closed. Runs once.
    fileprivate func channelEnded() {
        finish(nil)
        let wasCounted: Bool = state.withLock { s in
            guard !s.ended else { return false }
            s.ended = true
            return s.counted
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
