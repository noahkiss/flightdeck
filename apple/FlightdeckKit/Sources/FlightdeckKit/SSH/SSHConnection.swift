import Foundation
import NIOCore
import NIOPosix
import NIOSSH
import Synchronization

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
final class SSHConnection: Sendable {
    let channel: Channel
    let endpoint: SSHEndpoint
    let hostKey: NIOSSHPublicKey?
    /// The fingerprint of the key the host took, for a key-auth dial.
    let offered: String?

    init(channel: Channel, endpoint: SSHEndpoint, hostKey: NIOSSHPublicKey?, offered: String? = nil) {
        self.channel = channel
        self.endpoint = endpoint
        self.hostKey = hostKey
        self.offered = offered
    }

    var isActive: Bool { channel.isActive }

    func close() {
        channel.close(promise: nil)
    }

    /// Opens a session channel and runs `command` on it.
    func openSession(command: String, onEnd: @escaping @Sendable () -> Void, onFirstData: (@Sendable () -> Void)?) async throws -> SSHChannelStream {
        try await openSession(.exec(command), onEnd: onEnd, onFirstData: onFirstData)
    }

    /// What a session channel asks the host to run.
    enum SessionRequest {
        case exec(String)
        case subsystem(String)
    }

    /// Opens a session channel with an exec or subsystem request.
    func openSession(_ request: SessionRequest, onEnd: @escaping @Sendable () -> Void, onFirstData: (@Sendable () -> Void)?) async throws -> SSHChannelStream {
        let stream = SSHChannelStream(onEnd: onEnd, onFirstData: onFirstData)
        let parent = channel
        let execReply = parent.eventLoop.makePromise(of: Void.self)
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
                            // The handler is made here, on the child's loop, and
                            // stays there: it never crosses threads.
                            try child.pipeline.syncOperations.addHandler(stream.makeHandler(execReply: execReply))
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
        switch request {
        case .exec(let command):
            child.triggerUserOutboundEvent(SSHChannelRequestEvent.ExecRequest(command: command, wantReply: true), promise: nil)
        case .subsystem(let name):
            child.triggerUserOutboundEvent(SSHChannelRequestEvent.SubsystemRequest(subsystem: name, wantReply: true), promise: nil)
        }
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
        let c = try await dial(ep, user: user, auth: auth, check: check, timing: timing)
        // Auth succeeded with the key offered last; a key move records it.
        if let fp = c.offered { key.accepted(fingerprint: fp) }
        return c
    }

    /// Dials one endpoint with a ready user-auth delegate: TCP, key
    /// exchange, host key check (always before auth), auth. The delegate is
    /// Sendable: the channel initializer runs on the event loop.
    static func dial(_ ep: SSHEndpoint, user: String, auth: some NIOSSHClientUserAuthenticationDelegate & Sendable, check: HostKeyCheck, timing: SSHTiming) async throws -> SSHConnection {
        let loop = MultiThreadedEventLoopGroup.singleton.next()
        let watcher = ConnectionWatcher(ready: loop.makePromise(of: Void.self))
        let hostKeys = HostKeyValidator(check: check, endpoint: ep.description)
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
            watcher.abandon(e)
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
        return SSHConnection(channel: ch, endpoint: ep, hostKey: hostKeys.presented, offered: (auth as? ClientKeyAuth)?.lastOffered)
    }

    static func describe(_ error: Error) -> String {
        if let e = error as? IOError { return e.description }
        return String(describing: error)
    }
}

/// Watches the parent channel: authenticated, or the first error. The
/// pipeline calls it on the loop; the dial calls `abandon` and `fail` from
/// outside, so its state sits behind a mutex.
final class ConnectionWatcher: ChannelInboundHandler, Sendable {
    typealias InboundIn = Any
    let ready: EventLoopPromise<Void>

    private struct State {
        var done = false
        var firstError: Error?
    }

    private let state = Mutex(State())

    init(ready: EventLoopPromise<Void>) {
        self.ready = ready
    }

    /// The connect failed before the pipeline saw anything.
    func abandon(_ error: Error) {
        complete(error)
    }

    func userInboundEventTriggered(context: ChannelHandlerContext, event: Any) {
        if event is UserAuthSuccessEvent {
            complete(nil)
        }
        context.fireUserInboundEventTriggered(event)
    }

    func errorCaught(context: ChannelHandlerContext, error: Error) {
        state.withLock { if $0.firstError == nil { $0.firstError = error } }
        complete(error)
        context.close(promise: nil)
    }

    func channelInactive(context: ChannelHandlerContext) {
        let first = state.withLock { $0.firstError }
        complete(first ?? ConnectionClosed(detail: "the host closed the connection during the SSH handshake"))
        context.fireChannelInactive()
    }

    /// Fails the handshake from outside the pipeline (the timeout).
    func fail(_ error: Error, channel: Channel) {
        complete(error)
        channel.close(promise: nil)
    }

    private func complete(_ error: Error?) {
        let first: Bool = state.withLock { s in
            defer { s.done = true }
            return !s.done
        }
        guard first else { return }
        if let error { ready.fail(error) } else { ready.succeed(()) }
    }
}

/// Pins the host key, or stops at it for the user to confirm.
final class HostKeyValidator: NIOSSHClientServerAuthenticationDelegate, Sendable {
    let check: HostKeyCheck
    let endpoint: String
    private let seen = Mutex<NIOSSHPublicKey?>(nil)

    /// The key the host presented, once it did.
    var presented: NIOSSHPublicKey? { seen.withLock { $0 } }

    init(check: HostKeyCheck, endpoint: String) {
        self.check = check
        self.endpoint = endpoint
    }

    func validateHostKey(hostKey: NIOSSHPublicKey, validationCompletePromise: EventLoopPromise<Void>) {
        seen.withLock { $0 = hostKey }
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
final class ClientKeyAuth: NIOSSHClientUserAuthenticationDelegate, Sendable {
    let user: String
    let fingerprint: String
    let keys: [(fingerprint: String, key: NIOSSHPrivateKey)]
    let probe: Bool

    private struct State {
        var next = 0
        var lastOffered: String?
    }

    private let state = Mutex(State())

    /// The fingerprint of the key offered last: the one the host took, once
    /// auth succeeds. Read after the handshake.
    var lastOffered: String? { state.withLock { $0.lastOffered } }

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
        let k: (fingerprint: String, key: NIOSSHPrivateKey)? = state.withLock { s in
            guard s.next < keys.count, availableMethods.contains(.publicKey) else { return nil }
            let k = keys[s.next]
            s.next += 1
            s.lastOffered = k.fingerprint
            return k
        }
        guard let k else {
            nextChallengePromise.fail(SSHKeyRefused(user: user, fingerprint: fingerprint))
            return
        }
        nextChallengePromise.succeed(.init(username: user, serviceName: "", offer: .privateKey(.init(privateKey: k.key))))
    }
}
