import Foundation
import HabAPI
import NIOCore
import NIOPosix
import NIOSSH
import XCTest
@testable import FlightdeckKit

/// The in-app SSH carrier against an in-process SSH server on loopback that
/// bridges each channel to a fake hab.
final class SSHTransportTests: XCTestCase {
    private var server: TestSSHServer!
    private var key: GeneratedKey!

    override func setUp() async throws {
        server = TestSSHServer(hab: await sampleHab())
        try await server.start()
        key = try testKey()
        server.allow(key.publicKey)
    }

    override func tearDown() async throws {
        server.stop()
    }

    private func transport(pin: String? = nil, endpoints: [SSHEndpoint]? = nil) -> NIOSSHTransport {
        NIOSSHTransport(user: "tester", endpoints: endpoints ?? [server.endpoint], hostKeyFingerprint: pin ?? server.hostKeyFingerprint, key: key, timing: fastSSH())
    }

    func testHabOnlineOverSSH() async throws {
        let before = OpenStreams.live
        let t = transport()
        let conn = HabConnection(id: "h", transport: t, timing: fastTiming(), clientLabel: "test")
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        let sessions = await conn.state.store.sessions.count
        XCTAssertEqual(sessions, 2)
        _ = try await conn.call(HabMethods.habPing, PingParams())
        XCTAssertEqual(server.commands, ["gff-hab connect"])
        XCTAssertEqual(server.accepted, 1)
        await conn.stop()
        await eventually("stream closed") { OpenStreams.live == before }
        await eventually("ssh connection closed") { self.server.live == 0 }
        let n = await t.connectionCount()
        XCTAssertEqual(n, 0)
    }

    func testChangedHostKeyIsRefusedBeforeAuth() async throws {
        let other = SSHFingerprint.of(NIOSSHPrivateKey(ed25519Key: .init()).publicKey)
        let t = transport(pin: other)
        do {
            _ = try await t.open(.control)
            XCTFail("a changed host key was accepted")
        } catch let e as HostKeyChanged {
            XCTAssertEqual(e.expected, other)
            XCTAssertEqual(e.presented, server.hostKeyFingerprint)
            XCTAssertTrue(e.description.contains(other) && e.description.contains(server.hostKeyFingerprint))
        }
        XCTAssertEqual(server.authAttempts, 0, "no auth before the host key check")
        XCTAssertEqual(server.sessions, 0)
    }

    func testChangedHostKeyShowsOnTheHab() async throws {
        let other = SSHFingerprint.of(NIOSSHPrivateKey(ed25519Key: .init()).publicKey)
        let conn = HabConnection(id: "h", transport: transport(pin: other), timing: fastTiming())
        await conn.start()
        await eventually("offline") { await conn.state.status == .offline }
        let err = await conn.state.lastError ?? ""
        XCTAssertTrue(err.contains(other), err)
        XCTAssertTrue(err.contains(server.hostKeyFingerprint), err)
        await conn.stop()
        XCTAssertEqual(server.authAttempts, 0)
    }

    func testProbeReturnsHostKeyWithoutAuth() async throws {
        let k = try await NIOSSHTransport.probeHostKey(server.endpoint, user: "tester", key: key, timing: fastSSH())
        XCTAssertEqual(k, server.hostKey.publicKey)
        XCTAssertEqual(server.authAttempts, 0)
        await eventually("probe closed") { self.server.live == 0 }
    }

    func testRefusedKey() async throws {
        let stranger = try DeviceKeys(store: MemorySecretStore(), preferEnclave: false).defaultKey()
        let t = NIOSSHTransport(user: "tester", endpoints: [server.endpoint], hostKeyFingerprint: server.hostKeyFingerprint, key: stranger, timing: fastSSH())
        do {
            _ = try await t.open(.control)
            XCTFail("a key the server does not know was accepted")
        } catch let e as SSHKeyRefused {
            XCTAssertEqual(e.fingerprint, stranger.fingerprint)
            XCTAssertEqual(e.user, "tester")
        }
        XCTAssertEqual(server.sessions, 0)
    }

    func testControlAndDataShareOneConnection() async throws {
        let t = transport()
        let control = try await t.open(.control)
        let d1 = try await t.open(.data)
        let d2 = try await t.open(.data)
        XCTAssertEqual(server.accepted, 1)
        XCTAssertEqual(server.sessions, 3)
        let n = await t.connectionCount()
        XCTAssertEqual(n, 1)
        // The control channel still answers.
        let rpc = RPCClient(stream: control)
        await rpc.start()
        _ = try await rpc.call(HabMethods.habHello, HelloParams(client: ClientInfo(kind: .app, name: "t", version: "0"), minRevision: HabAPI.minRevision, revision: HabAPI.revision), timeout: .seconds(2))
        await rpc.close()
        d1.close()
        d2.close()
        await eventually("connection closed after the last channel") { self.server.live == 0 }
    }

    func testSessionLimitOpensASecondConnection() async throws {
        server.maxSessions = 2
        let t = transport()
        let control = try await t.open(.control)
        let d1 = try await t.open(.data)
        let d2 = try await t.open(.data)
        XCTAssertEqual(server.accepted, 2, "the refused channel moved to a second connection")
        XCTAssertEqual(server.sessions, 3)
        for s in [control, d1, d2] { s.close() }
        await eventually("all closed") { self.server.live == 0 }
    }

    func testNewControlStreamDialsAFreshConnection() async throws {
        let t = transport()
        let c1 = try await t.open(.control)
        let d1 = try await t.open(.data)
        c1.close()
        let c2 = try await t.open(.control)
        XCTAssertEqual(server.accepted, 2)
        let d2 = try await t.open(.data)
        XCTAssertEqual(server.accepted, 2, "data follows the newest control connection")
        d1.close()
        await eventually("old connection closes with its last channel") { self.server.live == 1 }
        c2.close()
        d2.close()
        await eventually("all closed") { self.server.live == 0 }
    }

    func testStaggeredEndpointsSkipADeadOne() async throws {
        let dead = try await closedPort()
        let t = transport(endpoints: [SSHEndpoint(host: "127.0.0.1", port: dead), server.endpoint])
        let s = try await t.open(.control)
        XCTAssertEqual(server.accepted, 1)
        s.close()
    }

    func testStaggerStartsTheNextEndpointWhileTheFirstHangs() async throws {
        // A listener that never speaks SSH: the first endpoint hangs in the handshake.
        let mute = try await ServerBootstrap(group: MultiThreadedEventLoopGroup.singleton)
            .childChannelInitializer { $0.eventLoop.makeSucceededFuture(()) }
            .bind(host: "127.0.0.1", port: 0).get()
        defer { mute.close(promise: nil) }
        let t = transport(endpoints: [SSHEndpoint(host: "127.0.0.1", port: mute.localAddress!.port!), server.endpoint])
        let clock = ContinuousClock()
        let start = clock.now
        let s = try await t.open(.control)
        XCTAssertLessThan(clock.now - start, .seconds(1), "the second endpoint won without waiting for the first's timeout")
        s.close()
    }

    func testAllEndpointsFailing() async throws {
        let dead = try await closedPort()
        let t = transport(endpoints: [SSHEndpoint(host: "127.0.0.1", port: dead), SSHEndpoint(host: "127.0.0.1", port: dead)])
        do {
            _ = try await t.open(.control)
            XCTFail("expected a failure")
        } catch {
            XCTAssertTrue("\(error)".contains("connect"), "\(error)")
        }
    }

    func testOneShotPing() async throws {
        let before = OpenStreams.live
        let r = try await HabOneShot.call(transport(), HabMethods.habPing, PingParams(), clientLabel: "action", deadline: .seconds(5))
        XCTAssertGreaterThan(r.elapsed, .zero)
        let pings = await server.hab.pings
        XCTAssertEqual(pings, 1)
        await eventually("ssh closed") { self.server.live == 0 }
        await eventually("stream closed") { OpenStreams.live == before }
    }

    func testOneShotKeepsItsDeadlineWhileTheHandshakeHangs() async throws {
        // A listener that never speaks SSH; the dial's own handshake limit is 15 s.
        let mute = try await ServerBootstrap(group: MultiThreadedEventLoopGroup.singleton)
            .childChannelInitializer { $0.eventLoop.makeSucceededFuture(()) }
            .bind(host: "127.0.0.1", port: 0).get()
        defer { mute.close(promise: nil) }
        let t = NIOSSHTransport(user: "tester", endpoints: [SSHEndpoint(host: "127.0.0.1", port: mute.localAddress!.port!)], hostKeyFingerprint: server.hostKeyFingerprint, key: key)
        let clock = ContinuousClock()
        let start = clock.now
        do {
            _ = try await HabOneShot.call(t, HabMethods.habPing, PingParams(), deadline: .milliseconds(500))
            XCTFail("expected the deadline")
        } catch {
            XCTAssertTrue("\(error)".contains("no answer"), "\(error)")
        }
        XCTAssertLessThan(clock.now - start, .seconds(5), "the 500 ms deadline ended the call; the margin is for a loaded runner")
        await eventually("no connection left") { await t.connectionCount() == 0 }
    }

    func testOneShotFailsFastOnAChangedKey() async throws {
        let other = SSHFingerprint.of(NIOSSHPrivateKey(ed25519Key: .init()).publicKey)
        do {
            _ = try await HabOneShot.call(transport(pin: other), HabMethods.habPing, PingParams(), deadline: .seconds(5))
            XCTFail("expected a refusal")
        } catch is HostKeyChanged {}
        let pings = await server.hab.pings
        XCTAssertEqual(pings, 0)
    }

    func testDataStreamOverSSH() async throws {
        await server.hab.terminals.add("%1")
        await server.hab.terminals.write("%1", Data("hello over ssh\r\n".utf8))
        let conn = HabConnection(id: "h", transport: transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        let ps = PaneStream(pane: "%1", connection: conn)
        let got = Got()
        await ps.start { e in await got.add(e) }
        await eventually("welcome on the data channel") { await got.welcomed }
        XCTAssertEqual(server.accepted, 1, "the data channel rides the control connection")
        XCTAssertEqual(server.sessions, 2)
        await ps.close()
        await conn.stop()
        await eventually("all closed") { self.server.live == 0 }
    }

    /// A port nothing listens on.
    private func closedPort() async throws -> Int {
        let ch = try await ServerBootstrap(group: MultiThreadedEventLoopGroup.singleton)
            .bind(host: "127.0.0.1", port: 0).get()
        let p = ch.localAddress!.port!
        try await ch.close()
        return p
    }
}

private actor Got {
    var welcomed = false
    func add(_ e: PaneStreamEvent) {
        if case .welcome = e { welcomed = true }
    }
}
