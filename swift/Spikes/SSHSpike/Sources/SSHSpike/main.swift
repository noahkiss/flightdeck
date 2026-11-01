// sshspike: prove swift-nio-ssh with a P-256 key against an sshd running `gff-hab connect`.
//
//   sshspike key --kind se|soft --out <file>
//       Make a P-256 key. "se" makes it in the Secure Enclave; the file then holds the
//       enclave's wrapped key blob, usable only on this device. "soft" writes the raw key.
//       Prints the OpenSSH public key line for authorized_keys.
//
//   sshspike connect --port <n> --key <file> --host-key <pub file> --command <cmd>
//                    [--host 127.0.0.1] [--user $USER] [--burst <n>]
//       Connect, pin the host key from <pub file> (an OpenSSH public key line), run <cmd>
//       on an exec channel, send hab.hello, read the welcome, then open two more channels at
//       once and do the same on each. --burst opens <n> more channels at once to probe the
//       server's session limit.

import CryptoKit
import Foundation
import NIOCore
import NIOPosix
import NIOSSH

struct SpikeError: Error, CustomStringConvertible {
    let description: String
    init(_ d: String) { description = d }
}

// MARK: - Arguments

func argValue(_ name: String, _ args: [String]) -> String? {
    guard let i = args.firstIndex(of: name), i + 1 < args.count else { return nil }
    return args[i + 1]
}

func required(_ name: String, _ args: [String]) throws -> String {
    guard let v = argValue(name, args) else { throw SpikeError("missing \(name)") }
    return v
}

// MARK: - Keys

// Key file format: one line "se" or "soft", then the base64 key data.
func makeKey(kind: String, out: String) throws -> NIOSSHPrivateKey {
    let data: Data
    let key: NIOSSHPrivateKey
    switch kind {
    case "se":
        guard SecureEnclave.isAvailable else { throw SpikeError("no Secure Enclave on this machine") }
        // No access-control flags: owner decision, Face ID is optional and off by default.
        let k = try SecureEnclave.P256.Signing.PrivateKey()
        data = k.dataRepresentation
        key = NIOSSHPrivateKey(secureEnclaveP256Key: k)
    case "soft":
        let k = P256.Signing.PrivateKey()
        data = k.rawRepresentation
        key = NIOSSHPrivateKey(p256Key: k)
    default:
        throw SpikeError("unknown key kind \(kind)")
    }
    let text = kind + "\n" + data.base64EncodedString() + "\n"
    FileManager.default.createFile(atPath: out, contents: Data(text.utf8),
                                   attributes: [.posixPermissions: 0o600])
    return key
}

func loadKey(_ path: String) throws -> (NIOSSHPrivateKey, String) {
    let lines = try String(contentsOfFile: path, encoding: .utf8).split(separator: "\n")
    guard lines.count >= 2, let data = Data(base64Encoded: String(lines[1])) else {
        throw SpikeError("bad key file")
    }
    switch lines[0] {
    case "se":
        return (NIOSSHPrivateKey(secureEnclaveP256Key:
            try SecureEnclave.P256.Signing.PrivateKey(dataRepresentation: data)), "se")
    case "soft":
        return (NIOSSHPrivateKey(p256Key: try P256.Signing.PrivateKey(rawRepresentation: data)), "soft")
    default:
        throw SpikeError("bad key kind")
    }
}

// MARK: - Delegates

final class KeyAuth: NIOSSHClientUserAuthenticationDelegate {
    let user: String
    let key: NIOSSHPrivateKey
    private var offered = false

    init(user: String, key: NIOSSHPrivateKey) {
        self.user = user
        self.key = key
    }

    func nextAuthenticationType(availableMethods: NIOSSHAvailableUserAuthenticationMethods,
                                nextChallengePromise: EventLoopPromise<NIOSSHUserAuthenticationOffer?>) {
        guard !offered, availableMethods.contains(.publicKey) else {
            nextChallengePromise.succeed(nil) // nothing else to offer: auth fails
            return
        }
        offered = true
        nextChallengePromise.succeed(.init(username: user, serviceName: "",
                                           offer: .privateKey(.init(privateKey: key))))
    }
}

struct HostKeyMismatch: Error, CustomStringConvertible {
    let presented: String
    var description: String { "host key mismatch: server presented \(presented)" }
}

// Host key pinning: accept exactly one key, refuse any other.
final class PinnedHostKey: NIOSSHClientServerAuthenticationDelegate {
    let pinned: NIOSSHPublicKey
    private(set) var presented: NIOSSHPublicKey?

    init(pinned: NIOSSHPublicKey) { self.pinned = pinned }

    func validateHostKey(hostKey: NIOSSHPublicKey, validationCompletePromise: EventLoopPromise<Void>) {
        presented = hostKey
        if hostKey == pinned {
            validationCompletePromise.succeed(())
        } else {
            validationCompletePromise.fail(HostKeyMismatch(presented: String(openSSHPublicKey: hostKey)))
        }
    }
}

// Records the first error on the parent channel and closes it.
final class ErrorCatcher: ChannelInboundHandler {
    typealias InboundIn = Any
    var error: Error?
    func errorCaught(context: ChannelHandlerContext, error: Error) {
        if self.error == nil { self.error = error }
        context.close(promise: nil)
    }
}

// MARK: - Exec channel with line reading

// Bridges one session channel: writes ByteBuffers as channel data, splits stdout into lines.
final class LineChannel: ChannelDuplexHandler {
    typealias InboundIn = SSHChannelData
    typealias InboundOut = Never
    typealias OutboundIn = ByteBuffer
    typealias OutboundOut = SSHChannelData

    let execReply: EventLoopPromise<Void>
    private var buffer: [UInt8] = []
    private var lines: [String] = []
    private var waiters: [EventLoopPromise<String>] = []
    private(set) var stderr = ""
    private(set) var exitStatus: Int?
    private var closed = false

    init(eventLoop: EventLoop) { execReply = eventLoop.makePromise() }

    func channelRead(context: ChannelHandlerContext, data: NIOAny) {
        let msg = unwrapInboundIn(data)
        guard case .byteBuffer(var buf) = msg.data, let bytes = buf.readBytes(length: buf.readableBytes) else { return }
        switch msg.type {
        case .channel:
            buffer.append(contentsOf: bytes)
            while let nl = buffer.firstIndex(of: 0x0A) {
                let line = String(decoding: buffer[..<nl], as: UTF8.self)
                buffer.removeSubrange(...nl)
                if waiters.isEmpty { lines.append(line) } else { waiters.removeFirst().succeed(line) }
            }
        case .stdErr:
            stderr += String(decoding: bytes, as: UTF8.self)
        default:
            break
        }
    }

    func userInboundEventTriggered(context: ChannelHandlerContext, event: Any) {
        switch event {
        case is ChannelSuccessEvent: execReply.succeed(())
        case is ChannelFailureEvent: execReply.fail(SpikeError("exec request refused"))
        case let s as SSHChannelRequestEvent.ExitStatus: exitStatus = s.exitStatus
        default: context.fireUserInboundEventTriggered(event)
        }
    }

    func channelInactive(context: ChannelHandlerContext) {
        closed = true
        let err = SpikeError("channel closed (exit \(exitStatus.map(String.init) ?? "?"), stderr: \(stderr))")
        execReply.fail(err)
        for w in waiters { w.fail(err) }
        waiters.removeAll()
        context.fireChannelInactive()
    }

    func write(context: ChannelHandlerContext, data: NIOAny, promise: EventLoopPromise<Void>?) {
        let buf = unwrapOutboundIn(data)
        context.write(wrapOutboundOut(SSHChannelData(type: .channel, data: .byteBuffer(buf))), promise: promise)
    }

    // Called on the channel's event loop.
    func nextLine(_ eventLoop: EventLoop) -> EventLoopFuture<String> {
        if !lines.isEmpty { return eventLoop.makeSucceededFuture(lines.removeFirst()) }
        if closed { return eventLoop.makeFailedFuture(SpikeError("channel closed; stderr: \(stderr)")) }
        let p = eventLoop.makePromise(of: String.self)
        waiters.append(p)
        return p.futureResult
    }
}

struct Exec {
    let channel: Channel
    let lines: LineChannel

    func send(_ line: String) -> EventLoopFuture<Void> {
        var buf = channel.allocator.buffer(capacity: line.utf8.count + 1)
        buf.writeString(line + "\n")
        return channel.writeAndFlush(buf)
    }

    func nextLine() -> EventLoopFuture<String> {
        channel.eventLoop.flatSubmit { lines.nextLine(channel.eventLoop) }
    }

    // Send one JSON-RPC request and read lines until the response with this id.
    func call(id: Int, method: String, params: String) -> EventLoopFuture<String> {
        let req = #"{"jsonrpc":"2.0","id":\#(id),"method":"\#(method)","params":\#(params)}"#
        return send(req).flatMap { readResponse(id: id) }
    }

    private func readResponse(id: Int) -> EventLoopFuture<String> {
        nextLine().flatMap { line in
            if let obj = try? JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any],
               (obj["id"] as? Int) == id {
                return channel.eventLoop.makeSucceededFuture(line)
            }
            return readResponse(id: id) // a notification; skip it
        }
    }
}

func openExec(_ parent: Channel, command: String) -> EventLoopFuture<Exec> {
    parent.pipeline.handler(type: NIOSSHHandler.self).flatMap { ssh in
        let created = parent.eventLoop.makePromise(of: Channel.self)
        let lines = LineChannel(eventLoop: parent.eventLoop)
        ssh.createChannel(created, channelType: .session) { child, type in
            guard case .session = type else {
                return child.eventLoop.makeFailedFuture(SpikeError("unexpected channel type"))
            }
            return child.setOption(ChannelOptions.allowRemoteHalfClosure, value: true).flatMap {
                child.pipeline.addHandler(lines)
            }
        }
        created.futureResult.whenFailure { lines.execReply.fail($0) }
        return created.futureResult.flatMap { child in
            child.triggerUserOutboundEvent(SSHChannelRequestEvent.ExecRequest(command: command, wantReply: true))
                .flatMap { lines.execReply.futureResult }
                .map { Exec(channel: child, lines: lines) }
        }
    }
}

// MARK: - Helpers

let helloParams = #"{"revision":1,"min_revision":1,"client":{"name":"sshspike","version":"0.1","kind":"app"},"label":"spike"}"#

func field(_ json: String, _ path: String...) -> String {
    var cur: Any? = try? JSONSerialization.jsonObject(with: Data(json.utf8))
    for p in path { cur = (cur as? [String: Any])?[p] }
    if let s = cur as? String { return s }
    if let n = cur as? NSNumber { return n.stringValue }
    return "?"
}

func ms(_ d: Duration) -> String {
    let c = d.components
    let v = Double(c.seconds) * 1000 + Double(c.attoseconds) / 1e15
    return String(format: "%.1f ms", v)
}

// MARK: - Commands

func runKey(_ args: [String]) throws {
    let key = try makeKey(kind: try required("--kind", args), out: try required("--out", args))
    print(String(openSSHPublicKey: key.publicKey), "sshspike")
}

func runConnect(_ args: [String]) throws {
    let host = argValue("--host", args) ?? "127.0.0.1"
    guard let port = Int(try required("--port", args)) else { throw SpikeError("bad --port") }
    let user = argValue("--user", args) ?? ProcessInfo.processInfo.environment["USER"] ?? ""
    let command = try required("--command", args)
    let burst = Int(argValue("--burst", args) ?? "0") ?? 0
    let pubLine = try String(contentsOfFile: try required("--host-key", args), encoding: .utf8)
    let pinned = try NIOSSHPublicKey(openSSHPublicKey:
        pubLine.split(separator: " ").prefix(2).joined(separator: " "))

    let clock = ContinuousClock()
    let t0 = clock.now
    let (key, kind) = try loadKey(try required("--key", args))
    let tKey = clock.now
    print("key: \(kind) P-256, loaded in \(ms(tKey - t0))")

    let group = MultiThreadedEventLoopGroup(numberOfThreads: 1)
    defer { try? group.syncShutdownGracefully() }
    let hostKeys = PinnedHostKey(pinned: pinned)
    let catcher = ErrorCatcher()
    let bootstrap = ClientBootstrap(group: group)
        .channelOption(ChannelOptions.socket(IPPROTO_TCP, TCP_NODELAY), value: 1)
        .channelInitializer { ch in
            ch.eventLoop.makeCompletedFuture {
                try ch.pipeline.syncOperations.addHandlers([
                    NIOSSHHandler(role: .client(.init(userAuthDelegate: KeyAuth(user: user, key: key),
                                                      serverAuthDelegate: hostKeys)),
                                  allocator: ch.allocator, inboundChildChannelInitializer: nil),
                    catcher,
                ])
            }
        }

    let tStart = clock.now
    let parent = try bootstrap.connect(host: host, port: port).wait()
    let tTCP = clock.now
    let first: Exec
    do {
        first = try openExec(parent, command: command).wait()
    } catch {
        // The handshake error (host key, auth) lands on the parent; prefer it.
        throw catcher.error ?? error
    }
    let tExec = clock.now
    let welcome = try first.call(id: 1, method: "hab.hello", params: helloParams).wait()
    let tReply = clock.now
    print("host key: pinned key matched (\(String(openSSHPublicKey: hostKeys.presented!).prefix(40))...)")
    print("tcp connect:                 \(ms(tTCP - tStart))")
    print("ssh handshake+auth+exec:     \(ms(tExec - tTCP))")
    print("hab.hello round trip:        \(ms(tReply - tExec))")
    print("connect to first reply:      \(ms(tReply - tStart))")
    print("channel 1 welcome: revision=\(field(welcome, "result", "revision")) client_id=\(field(welcome, "result", "client_id")) device_id=\(field(welcome, "result", "device_id"))")

    // Two more channels at once on the same connection.
    let t2 = clock.now
    let a = openExec(parent, command: command)
    let b = openExec(parent, command: command)
    let execs = try EventLoopFuture.whenAllSucceed([a, b], on: parent.eventLoop).wait()
    let replies = try EventLoopFuture.whenAllSucceed(
        execs.map { $0.call(id: 1, method: "hab.hello", params: helloParams) }, on: parent.eventLoop).wait()
    let t3 = clock.now
    for (i, r) in replies.enumerated() {
        print("channel \(i + 2) welcome: client_id=\(field(r, "result", "client_id"))")
    }
    print("two concurrent channels, open to both welcomes: \(ms(t3 - t2))")

    // Channel 1 still answers after the others opened.
    let ping = try first.call(id: 2, method: "hab.ping", params: "{}").wait()
    print("channel 1 hab.ping: \(ping.contains("\"result\"") ? "ok" : ping)")

    if burst > 0 {
        let futures = (0..<burst).map { _ in openExec(parent, command: command) }
        var ok = 0
        var failures: [String] = []
        for f in futures {
            do {
                let e = try f.wait()
                _ = try e.call(id: 1, method: "hab.hello", params: helloParams).wait()
                ok += 1
            } catch { failures.append("\(error)") }
        }
        print("burst of \(burst) more channels (3 already open): \(ok) ok, \(failures.count) failed")
        if let f = failures.first { print("  first failure: \(f)") }
    }

    try parent.close().wait()
}

setvbuf(stdout, nil, _IOLBF, 0)
let args = Array(CommandLine.arguments.dropFirst())
do {
    switch args.first {
    case "key": try runKey(args)
    case "connect": try runConnect(args)
    default:
        FileHandle.standardError.write(Data("usage: sshspike key|connect ...\n".utf8))
        exit(2)
    }
} catch {
    FileHandle.standardError.write(Data("error: \(error)\n".utf8))
    exit(1)
}
