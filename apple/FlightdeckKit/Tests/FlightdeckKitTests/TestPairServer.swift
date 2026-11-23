// Process and the real /bin/sh exist only on macOS; the iOS Simulator run skips these.
#if os(macOS)
import Foundation
import NIOCore
import NIOPosix
import NIOSSH
import Synchronization
@testable import FlightdeckKit

/// An SSH server on a loopback port for the pairing and password setup
/// tests. It stands in for the hab's pairing window (`flightdeck-pair`, the
/// answers scripted by the test) or for a host's sshd with password auth,
/// whose `sh -s` runs the real `/bin/sh` with a temporary `HOME`.
final class TestPairServer: Sendable {
    /// Gets the request line; writes answer lines with `write`, ends with `close`.
    typealias PairHandler = @Sendable (_ request: Data, _ write: @escaping @Sendable (String) -> Void, _ close: @escaping @Sendable () -> Void) -> Void

    let hostKey: NIOSSHPrivateKey
    let methods: NIOSSHAvailableUserAuthenticationMethods
    var password: String? {
        get { state.withLock { $0.password } }
        set { state.withLock { $0.password = newValue } }
    }
    var pairHandler: PairHandler? {
        get { state.withLock { $0.pairHandler } }
        set { state.withLock { $0.pairHandler = newValue } }
    }
    /// The `HOME` the shell runs with.
    var home: URL? {
        get { state.withLock { $0.home } }
        set { state.withLock { $0.home = newValue } }
    }
    /// Never answer the subsystem request: a window that takes auth, then stalls.
    var silent: Bool {
        get { state.withLock { $0.silent } }
        set { state.withLock { $0.silent = newValue } }
    }

    fileprivate struct State {
        var password: String?
        var pairHandler: PairHandler?
        var home: URL?
        var silent = false
        var channel: Channel?
        var keys: [NIOSSHPublicKey] = []
        var users: [String] = []
        var passwords: [String] = []
        var subsystems: [String] = []
        var execs: [String] = []
        var requests: [Data] = []
    }

    private let state = Mutex(State())

    init(methods: NIOSSHAvailableUserAuthenticationMethods = .publicKey, hostKey: NIOSSHPrivateKey = NIOSSHPrivateKey(ed25519Key: .init())) {
        self.methods = methods
        self.hostKey = hostKey
    }

    var hostKeyFingerprint: String { SSHFingerprint.of(hostKey.publicKey) }
    var port: Int { state.withLock { $0.channel }?.localAddress?.port ?? 0 }
    var endpoint: SSHEndpoint { SSHEndpoint(host: "127.0.0.1", port: port) }

    var keys: [NIOSSHPublicKey] { state.withLock { $0.keys } }
    var users: [String] { state.withLock { $0.users } }
    var passwords: [String] { state.withLock { $0.passwords } }
    var subsystems: [String] { state.withLock { $0.subsystems } }
    var execs: [String] { state.withLock { $0.execs } }
    var requests: [Data] { state.withLock { $0.requests } }

    func start() async throws {
        let ch = try await ServerBootstrap(group: MultiThreadedEventLoopGroup.singleton)
            .serverChannelOption(ChannelOptions.socketOption(.so_reuseaddr), value: 1)
            .childChannelInitializer { [self] ch in
                ch.eventLoop.makeCompletedFuture {
                    try ch.pipeline.syncOperations.addHandler(NIOSSHHandler(
                        role: .server(.init(hostKeys: [hostKey], userAuthDelegate: Auth(server: self))),
                        allocator: ch.allocator,
                        inboundChildChannelInitializer: { child, type in
                            guard case .session = type else {
                                return child.eventLoop.makeFailedFuture(TransportError("not a session"))
                            }
                            return child.eventLoop.makeCompletedFuture {
                                try child.syncOptions?.setOption(ChannelOptions.allowRemoteHalfClosure, value: true)
                                try child.pipeline.syncOperations.addHandler(Session(server: self))
                            }
                        }
                    ))
                }
            }
            .bind(host: "127.0.0.1", port: 0).get()
        state.withLock { $0.channel = ch }
    }

    func stop() {
        state.withLock { $0.channel }?.close(promise: nil)
    }

    fileprivate func record(_ f: (inout State) -> Void) { state.withLock { f(&$0) } }

    /// Its only state is the server, which is Sendable.
    private final class Auth: NIOSSHServerUserAuthenticationDelegate, Sendable {
        let server: TestPairServer
        init(server: TestPairServer) { self.server = server }
        var supportedAuthenticationMethods: NIOSSHAvailableUserAuthenticationMethods { server.methods }
        func requestReceived(request: NIOSSHUserAuthenticationRequest, responsePromise: EventLoopPromise<NIOSSHUserAuthenticationOutcome>) {
            switch request.request {
            case .publicKey(let pk) where server.methods.contains(.publicKey):
                server.record { $0.keys.append(pk.publicKey); $0.users.append(request.username) }
                responsePromise.succeed(.success)
            case .password(let p) where server.methods.contains(.password):
                server.record { $0.passwords.append(p.password); $0.users.append(request.username) }
                responsePromise.succeed(p.password == server.password ? .success : .failure)
            default:
                responsePromise.succeed(.failure)
            }
        }
    }

    /// Not Sendable: it lives on its channel's event loop, as NIO handlers do.
    private final class Session: ChannelInboundHandler {
        typealias InboundIn = SSHChannelData
        typealias OutboundOut = SSHChannelData

        let server: TestPairServer
        private var mode: String?
        private var input = Data()
        private var handled = false
        private var shell: Process?
        private var stdin: Pipe?

        init(server: TestPairServer) { self.server = server }

        func userInboundEventTriggered(context: ChannelHandlerContext, event: Any) {
            let ch = context.channel
            switch event {
            case let s as SSHChannelRequestEvent.SubsystemRequest:
                server.record { $0.subsystems.append(s.subsystem) }
                if server.silent { return }
                let ok = mode == nil && s.subsystem == Pairing.subsystem && server.pairHandler != nil
                if ok { mode = "pair" }
                if s.wantReply { ch.triggerUserOutboundEvent(ok ? ChannelSuccessEvent() : ChannelFailureEvent(), promise: nil) }
            case let e as SSHChannelRequestEvent.ExecRequest:
                server.record { $0.execs.append(e.command) }
                let ok = mode == nil && e.command == "sh -s" && server.home != nil
                if ok { startShell(ch) }
                if e.wantReply { ch.triggerUserOutboundEvent(ok ? ChannelSuccessEvent() : ChannelFailureEvent(), promise: nil) }
            case ChannelEvent.inputClosed:
                try? stdin?.fileHandleForWriting.close()
            default:
                context.fireUserInboundEventTriggered(event)
            }
        }

        func channelRead(context: ChannelHandlerContext, data: NIOAny) {
            let msg = unwrapInboundIn(data)
            guard case .channel = msg.type, case .byteBuffer(let buf) = msg.data else { return }
            let d = Data(buf.readableBytesView)
            if mode == "shell" {
                try? stdin?.fileHandleForWriting.write(contentsOf: d)
                return
            }
            guard mode == "pair", !handled else { return }
            input.append(d)
            guard let nl = input.firstIndex(of: 0x0A) else { return }
            handled = true
            let line = Data(input[..<nl])
            server.record { $0.requests.append(line) }
            let ch = context.channel
            server.pairHandler?(line, { text in
                let buf = ch.allocator.buffer(string: text + "\n")
                ch.writeAndFlush(SSHChannelData(type: .channel, data: .byteBuffer(buf)), promise: nil)
            }, {
                ch.close(promise: nil)
            })
        }

        private func startShell(_ ch: Channel) {
            mode = "shell"
            let p = Process()
            p.executableURL = URL(fileURLWithPath: "/bin/sh")
            p.arguments = ["-s"]
            p.environment = ["HOME": server.home!.path, "PATH": "/usr/bin:/bin"]
            let inP = Pipe(), outP = Pipe()
            p.standardInput = inP
            p.standardOutput = outP
            p.standardError = FileHandle.nullDevice
            stdin = inP
            shell = p
            do { try p.run() } catch {
                ch.close(promise: nil)
                return
            }
            Thread.detachNewThread {
                let out = outP.fileHandleForReading.readDataToEndOfFile()
                p.waitUntilExit()
                ch.eventLoop.execute {
                    let buf = ch.allocator.buffer(bytes: out)
                    ch.writeAndFlush(SSHChannelData(type: .channel, data: .byteBuffer(buf)), promise: nil)
                    ch.triggerUserOutboundEvent(SSHChannelRequestEvent.ExitStatus(exitStatus: Int(p.terminationStatus)), promise: nil)
                    ch.close(promise: nil)
                }
            }
        }
    }
}
#endif
