import CryptoKit
import Foundation
import NIOCore
import NIOPosix
import NIOSSH
@testable import FlightdeckKit

/// An SSH server on a loopback port for the SSH carrier's tests. Every exec
/// channel bridges to a `FakeHab`, as `gff-hab connect` bridges to the hab's
/// socket: the first byte tells a data connection (1) from a control one.
final class TestSSHServer: @unchecked Sendable {
    let hostKey: NIOSSHPrivateKey
    let hab: FakeHab
    private let habTransport: FakeTransport
    private let lock = NSLock()
    private var allowed: Set<NIOSSHPublicKey> = []
    private var channel: Channel?
    private var _accepted = 0
    private var _authAttempts = 0
    private var _sessions = 0
    private var _live = 0
    private var _commands: [String] = []
    /// Sessions allowed per connection, like OpenSSH's MaxSessions.
    var maxSessions = 10

    init(hab: FakeHab, hostKey: NIOSSHPrivateKey = NIOSSHPrivateKey(ed25519Key: .init())) {
        self.hab = hab
        self.hostKey = hostKey
        self.habTransport = hab.transport(endpoint: "ssh-test")
    }

    var hostKeyFingerprint: String { SSHFingerprint.of(hostKey.publicKey) }

    func allow(_ key: NIOSSHPublicKey) { lock.withLock { _ = allowed.insert(key) } }

    /// TCP connections accepted.
    var accepted: Int { lock.withLock { _accepted } }
    /// Public key auth requests seen.
    var authAttempts: Int { lock.withLock { _authAttempts } }
    /// Session channels opened, over all connections.
    var sessions: Int { lock.withLock { _sessions } }
    /// TCP connections open now.
    var live: Int { lock.withLock { _live } }
    var commands: [String] { lock.withLock { _commands } }

    var port: Int { channel?.localAddress?.port ?? 0 }
    var endpoint: SSHEndpoint { SSHEndpoint(host: "127.0.0.1", port: port) }

    func start() async throws {
        let ch = try await ServerBootstrap(group: MultiThreadedEventLoopGroup.singleton)
            .serverChannelOption(ChannelOptions.socketOption(.so_reuseaddr), value: 1)
            .childChannelInitializer { [self] ch in
                lock.withLock { _accepted += 1; _live += 1 }
                ch.closeFuture.whenComplete { _ in self.lock.withLock { self._live -= 1 } }
                let perConn = SessionCount()
                return ch.eventLoop.makeCompletedFuture {
                    try ch.pipeline.syncOperations.addHandler(NIOSSHHandler(
                        role: .server(.init(hostKeys: [hostKey], userAuthDelegate: Auth(server: self))),
                        allocator: ch.allocator,
                        inboundChildChannelInitializer: { child, type in
                            guard case .session = type else {
                                return child.eventLoop.makeFailedFuture(TransportError("not a session"))
                            }
                            let ok: Bool = self.lock.withLock {
                                guard perConn.n < self.maxSessions else { return false }
                                perConn.n += 1
                                self._sessions += 1
                                return true
                            }
                            guard ok else {
                                return child.eventLoop.makeFailedFuture(TransportError("session limit"))
                            }
                            child.closeFuture.whenComplete { _ in self.lock.withLock { perConn.n -= 1 } }
                            return child.eventLoop.makeCompletedFuture {
                                try child.pipeline.syncOperations.addHandler(Bridge(server: self))
                            }
                        }
                    ))
                }
            }
            .bind(host: "127.0.0.1", port: 0).get()
        channel = ch
    }

    func stop() {
        channel?.close(promise: nil)
    }

    fileprivate func sawAuth() { lock.withLock { _authAttempts += 1 } }
    fileprivate func isAllowed(_ k: NIOSSHPublicKey) -> Bool { lock.withLock { allowed.contains(k) } }
    fileprivate func sawCommand(_ c: String) { lock.withLock { _commands.append(c) } }
    fileprivate func openHab(_ kind: StreamKind) async throws -> FakeStream {
        // The fake transport hands back its client end as a FakeStream.
        try await habTransport.open(kind) as! FakeStream
    }

    private final class SessionCount: @unchecked Sendable { var n = 0 }

    private final class Auth: NIOSSHServerUserAuthenticationDelegate {
        let server: TestSSHServer
        init(server: TestSSHServer) { self.server = server }
        var supportedAuthenticationMethods: NIOSSHAvailableUserAuthenticationMethods { .publicKey }
        func requestReceived(request: NIOSSHUserAuthenticationRequest, responsePromise: EventLoopPromise<NIOSSHUserAuthenticationOutcome>) {
            server.sawAuth()
            if case .publicKey(let pk) = request.request, server.isAllowed(pk.publicKey) {
                responsePromise.succeed(.success)
            } else {
                responsePromise.succeed(.failure)
            }
        }
    }

    /// One exec channel bridged to the fake hab.
    private final class Bridge: ChannelInboundHandler, @unchecked Sendable {
        typealias InboundIn = SSHChannelData
        typealias OutboundOut = SSHChannelData

        let server: TestSSHServer
        private var hab: FakeStream?
        private var pending: [Data] = []
        private var opening = false
        private var closed = false

        init(server: TestSSHServer) { self.server = server }

        func userInboundEventTriggered(context: ChannelHandlerContext, event: Any) {
            if let exec = event as? SSHChannelRequestEvent.ExecRequest {
                server.sawCommand(exec.command)
                if exec.wantReply {
                    context.channel.triggerUserOutboundEvent(ChannelSuccessEvent(), promise: nil)
                }
                return
            }
            context.fireUserInboundEventTriggered(event)
        }

        func channelRead(context: ChannelHandlerContext, data: NIOAny) {
            let msg = unwrapInboundIn(data)
            guard case .channel = msg.type, case .byteBuffer(let buf) = msg.data else { return }
            let d = Data(buf.readableBytesView)
            if let hab {
                try? hab.send(d)
                return
            }
            pending.append(d)
            guard !opening, let first = d.first else { return }
            opening = true
            let kind: StreamKind = first == 1 ? .data : .control
            let ch = context.channel
            let loop = context.eventLoop
            Task {
                do {
                    let s = try await self.server.openHab(kind)
                    loop.execute { self.attach(s, channel: ch) }
                } catch {
                    ch.close(promise: nil)
                }
            }
        }

        private func attach(_ s: FakeStream, channel ch: Channel) {
            if closed {
                s.close()
                return
            }
            hab = s
            for d in pending { try? s.send(d) }
            pending.removeAll()
            Task {
                while let d = try? await s.read() {
                    let buf = ch.allocator.buffer(bytes: d)
                    try? await ch.writeAndFlush(SSHChannelData(type: .channel, data: .byteBuffer(buf))).get()
                }
                ch.close(promise: nil)
            }
        }

        func channelInactive(context: ChannelHandlerContext) {
            closed = true
            hab?.close()
            context.fireChannelInactive()
        }
    }
}

/// A software device key for tests.
func testKey() throws -> GeneratedKey {
    try DeviceKeys(store: MemorySecretStore(), preferEnclave: false).defaultKey()
}

/// Fast SSH timings for tests.
func fastSSH() -> SSHTiming {
    var t = SSHTiming()
    t.stagger = .milliseconds(100)
    t.connectTimeout = .seconds(2)
    t.handshakeTimeout = .seconds(2)
    return t
}
