import Foundation
import NIOCore
import NIOPosix
import NIOSSH

/// How the client checks the host key.
public enum HostKeyCheck: Sendable, Equatable {
    /// Accept only the key with this fingerprint.
    case pinned(String)
    /// Nothing pinned yet: stop at the host key and report it
    /// (`HostKeyUnconfirmed`) so the user can confirm it first.
    case probe
}

/// Timings of one SSH dial.
public struct SSHTiming: Sendable {
    /// Each endpoint after the first starts this long after the previous
    /// one, unless the previous one failed sooner (app.md 10.1).
    public var stagger: Duration = .milliseconds(300)
    /// TCP connect, per endpoint.
    public var connectTimeout: Duration = .seconds(10)
    /// From TCP connected to authenticated.
    public var handshakeTimeout: Duration = .seconds(15)

    public init() {}
}

/// One authenticated SSH connection. The pool that made it owns the counts.
final class SSHConnection: @unchecked Sendable {
    let channel: Channel
    let endpoint: SSHEndpoint
    let hostKey: NIOSSHPublicKey?
    /// The pool generation it belongs to; older ones take no new channels.
    var generation = 0
    /// Open session channels.
    var children = 0
    /// The host refused a channel (OpenSSH's MaxSessions): take no more.
    var full = false

    init(channel: Channel, endpoint: SSHEndpoint, hostKey: NIOSSHPublicKey?) {
        self.channel = channel
        self.endpoint = endpoint
        self.hostKey = hostKey
    }

    var isActive: Bool { channel.isActive }

    func close() {
        channel.close(promise: nil)
    }

    /// Opens a session channel and runs `command` on it.
    func openSession(command: String, onEnd: @escaping @Sendable () -> Void, onFirstData: (@Sendable () -> Void)?) async throws -> SSHChannelStream {
        let stream = SSHChannelStream(onEnd: onEnd, onFirstData: onFirstData)
        let parent = channel
        let execReply = parent.eventLoop.makePromise(of: Void.self)
        let handler = stream.makeHandler(execReply: execReply)
        let child: Channel
        do {
            child = try await parent.eventLoop.flatSubmit { () -> EventLoopFuture<Channel> in
                let created = parent.eventLoop.makePromise(of: Channel.self)
                do {
                    let ssh = try parent.pipeline.syncOperations.handler(type: NIOSSHHandler.self)
                    ssh.createChannel(created, channelType: .session) { child, _ in
                        child.eventLoop.makeCompletedFuture {
                            try child.syncOptions?.setOption(ChannelOptions.allowRemoteHalfClosure, value: true)
                            try child.syncOptions?.setOption(ChannelOptions.autoRead, value: false)
                            try child.pipeline.syncOperations.addHandler(handler)
                        }
                    }
                } catch {
                    created.fail(error)
                }
                return created.futureResult
            }.get()
        } catch {
            execReply.fail(error)
            onEnd()
            throw error
        }
        child.triggerUserOutboundEvent(SSHChannelRequestEvent.ExecRequest(command: command, wantReply: true), promise: nil)
        do {
            try await execReply.futureResult.get()
        } catch {
            child.close(promise: nil)
            throw error
        }
        stream.attach(child)
        return stream
    }

    /// Dials one endpoint: TCP, key exchange, host key check, auth.
    static func dial(_ ep: SSHEndpoint, user: String, key: SSHKeySource, check: HostKeyCheck, timing: SSHTiming) async throws -> SSHConnection {
        let loop = MultiThreadedEventLoopGroup.singleton.next()
        let watcher = ConnectionWatcher(ready: loop.makePromise(of: Void.self))
        let hostKeys = HostKeyValidator(check: check, endpoint: ep.description)
        // Unlock the key here, off the event loop: a Face ID prompt must not
        // hold the loop every hab shares. The probe never signs.
        var signing: [(fingerprint: String, key: NIOSSHPrivateKey)] = []
        if check != .probe {
            do {
                signing = try await key.signingKeys()
            } catch {
                throw DeviceKeys.explain(error)
            }
        }
        let auth = ClientKeyAuth(user: user, fingerprint: key.fingerprint, keys: signing, probe: check == .probe)
        let bootstrap = ClientBootstrap(group: loop)
            .connectTimeout(.nanoseconds(Int64(timing.connectTimeout.components.seconds) * 1_000_000_000 + timing.connectTimeout.components.attoseconds / 1_000_000_000))
            .channelOption(ChannelOptions.socket(IPPROTO_TCP, TCP_NODELAY), value: 1)
            .channelInitializer { ch in
                ch.eventLoop.makeCompletedFuture {
                    try ch.pipeline.syncOperations.addHandlers([
                        NIOSSHHandler(
                            role: .client(.init(userAuthDelegate: auth, serverAuthDelegate: hostKeys)),
                            allocator: ch.allocator,
                            inboundChildChannelInitializer: nil
                        ),
                        watcher,
                    ])
                }
            }
        let ch: Channel
        do {
            ch = try await bootstrap.connect(host: ep.host, port: ep.port).get()
        } catch {
            let e = TransportError("connect \(ep): \(Self.describe(error))")
            loop.execute { watcher.abandon(e) }
            throw e
        }
        let ready = watcher.ready
        let hs = timing.handshakeTimeout
        let timer = ch.eventLoop.scheduleTask(in: .nanoseconds(Int64(hs.components.seconds) * 1_000_000_000 + hs.components.attoseconds / 1_000_000_000)) {
            watcher.fail(SSHHandshakeTimeout(endpoint: ep.description), channel: ch)
        }
        do {
            try await withTaskCancellationHandler {
                try await ready.futureResult.get()
            } onCancel: {
                ch.close(promise: nil)
            }
        } catch {
            timer.cancel()
            ch.close(promise: nil)
            throw error
        }
        timer.cancel()
        // Auth succeeded with the key offered last; a key move records it.
        if let fp = auth.lastOffered { key.accepted(fingerprint: fp) }
        return SSHConnection(channel: ch, endpoint: ep, hostKey: hostKeys.presented)
    }

    static func describe(_ error: Error) -> String {
        if let e = error as? IOError { return e.description }
        return String(describing: error)
    }
}

/// Watches the parent channel: authenticated, or the first error.
final class ConnectionWatcher: ChannelInboundHandler, @unchecked Sendable {
    typealias InboundIn = Any
    let ready: EventLoopPromise<Void>
    private var done = false

    init(ready: EventLoopPromise<Void>) {
        self.ready = ready
    }

    /// The connect failed before the pipeline saw anything. On the loop.
    func abandon(_ error: Error) {
        complete(error)
    }
    private var firstError: Error?

    func userInboundEventTriggered(context: ChannelHandlerContext, event: Any) {
        if event is UserAuthSuccessEvent {
            complete(nil)
        }
        context.fireUserInboundEventTriggered(event)
    }

    func errorCaught(context: ChannelHandlerContext, error: Error) {
        if firstError == nil { firstError = error }
        complete(error)
        context.close(promise: nil)
    }

    func channelInactive(context: ChannelHandlerContext) {
        complete(firstError ?? ConnectionClosed(detail: "the host closed the connection during the SSH handshake"))
        context.fireChannelInactive()
    }

    /// Fails the handshake from outside the pipeline (the timeout).
    func fail(_ error: Error, channel: Channel) {
        channel.eventLoop.execute {
            self.complete(error)
            channel.close(promise: nil)
        }
    }

    private func complete(_ error: Error?) {
        guard !done else { return }
        done = true
        if let error { ready.fail(error) } else { ready.succeed(()) }
    }
}

/// Pins the host key, or stops at it for the user to confirm.
final class HostKeyValidator: NIOSSHClientServerAuthenticationDelegate, @unchecked Sendable {
    let check: HostKeyCheck
    let endpoint: String
    private(set) var presented: NIOSSHPublicKey?

    init(check: HostKeyCheck, endpoint: String) {
        self.check = check
        self.endpoint = endpoint
    }

    func validateHostKey(hostKey: NIOSSHPublicKey, validationCompletePromise: EventLoopPromise<Void>) {
        presented = hostKey
        switch check {
        case .pinned(let fp):
            let got = SSHFingerprint.of(hostKey)
            if got == fp && SSHFingerprint.isValid(fp) {
                validationCompletePromise.succeed(())
            } else {
                validationCompletePromise.fail(HostKeyChanged(expected: fp, presented: got, endpoint: endpoint))
            }
        case .probe:
            validationCompletePromise.fail(HostKeyUnconfirmed(key: hostKey, endpoint: endpoint))
        }
    }
}

/// Offers each key once, in order: one device key, or during a key move the
/// new key, then the old one (transport.md 7.2). A request after the last
/// offer means the host refused them all.
final class ClientKeyAuth: NIOSSHClientUserAuthenticationDelegate, @unchecked Sendable {
    let user: String
    let fingerprint: String
    let keys: [(fingerprint: String, key: NIOSSHPrivateKey)]
    let probe: Bool
    private var next = 0
    /// The fingerprint of the key offered last: the one the host took, once
    /// auth succeeds. Read after the handshake.
    private(set) var lastOffered: String?

    init(user: String, fingerprint: String, keys: [(fingerprint: String, key: NIOSSHPrivateKey)], probe: Bool = false) {
        self.user = user
        self.fingerprint = fingerprint
        self.keys = keys
        self.probe = probe
    }

    func nextAuthenticationType(availableMethods: NIOSSHAvailableUserAuthenticationMethods, nextChallengePromise: EventLoopPromise<NIOSSHUserAuthenticationOffer?>) {
        if probe || keys.isEmpty {
            nextChallengePromise.fail(TransportError("no key to sign with"))
            return
        }
        guard next < keys.count, availableMethods.contains(.publicKey) else {
            nextChallengePromise.fail(SSHKeyRefused(user: user, fingerprint: fingerprint))
            return
        }
        let k = keys[next]
        next += 1
        lastOffered = k.fingerprint
        nextChallengePromise.succeed(.init(username: user, serviceName: "", offer: .privateKey(.init(privateKey: k.key))))
    }
}
