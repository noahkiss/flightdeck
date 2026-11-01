import Foundation
import HabAPI
import NIOSSH
import XCTest
@testable import FlightdeckKit

/// The in-app SSH carrier against a real sshd running `gff-hab connect`.
/// `apple/scripts/ssh-live-check.sh` sets it up on a loopback port and runs
/// this test. Skipped unless the environment names the sshd:
///
/// - `FLIGHTDECK_LIVE_NIO_PORT`: the sshd's port on 127.0.0.1
///   (`FLIGHTDECK_LIVE_NIO_HOST` overrides the address).
/// - `FLIGHTDECK_LIVE_NIO_HOSTKEY`: the sshd's public host key file.
/// - `FLIGHTDECK_LIVE_NIO_AUTHKEYS`: the authorized keys file the sshd reads;
///   the test writes its new key there.
/// - `FLIGHTDECK_LIVE_NIO_COMMAND`: the remote command.
/// - `FLIGHTDECK_LIVE_NIO_USER`: the user (default `$USER`).
/// - `FLIGHTDECK_LIVE_NIO_PANE`: when set, also make a pane running this
///   shell command and read its output over a data channel.
/// - `FLIGHTDECK_LIVE_NIO_IMPORT_KEY` and `FLIGHTDECK_LIVE_NIO_IMPORT_PASS`:
///   a passphrase-protected OpenSSH key file the sshd already trusts and its
///   passphrase; the import test reads it, stores it and pings with it.
final class SSHLiveTests: XCTestCase {
    private var env: [String: String] { ProcessInfo.processInfo.environment }

    func testLiveNIOSSH() async throws {
        guard let portText = env["FLIGHTDECK_LIVE_NIO_PORT"], let port = Int(portText) else {
            throw XCTSkip("FLIGHTDECK_LIVE_NIO_PORT not set")
        }
        let host = env["FLIGHTDECK_LIVE_NIO_HOST"] ?? "127.0.0.1"
        let user = env["FLIGHTDECK_LIVE_NIO_USER"] ?? env["USER"] ?? ""
        let command = try XCTUnwrap(env["FLIGHTDECK_LIVE_NIO_COMMAND"])
        let hostKeyLine = try String(contentsOfFile: try XCTUnwrap(env["FLIGHTDECK_LIVE_NIO_HOSTKEY"]), encoding: .utf8)
        let hostKey = try NIOSSHPublicKey(openSSHPublicKey: hostKeyLine.split(separator: " ").prefix(2).joined(separator: " "))
        let authKeys = try XCTUnwrap(env["FLIGHTDECK_LIVE_NIO_AUTHKEYS"])
        let ep = SSHEndpoint(host: host, port: port)

        // The device key: the Secure Enclave when it exists and the screen is unlocked.
        let key: GeneratedKey
        do {
            key = try DeviceKeys(store: MemorySecretStore()).defaultKey()
        } catch {
            print("live-nio: no Secure Enclave key (\(DeviceKeys.explain(error))); using a software key")
            key = try DeviceKeys(store: MemorySecretStore(), preferEnclave: false).defaultKey()
        }
        try appendLine(key.authorizedKeysLine(comment: "flightdeck-live-check"), to: authKeys)
        print("live-nio: key \(key.kind.rawValue) \(key.fingerprint)")

        // First contact: the probe shows the host key, which the user confirms.
        let probed = try await NIOSSHTransport.probeHostKey(ep, user: user, key: key)
        XCTAssertEqual(probed, hostKey)

        // A different pinned key is refused.
        let wrong = SSHFingerprint.of(NIOSSHPrivateKey(ed25519Key: .init()).publicKey)
        do {
            _ = try await NIOSSHTransport(user: user, endpoints: [ep], hostKeyFingerprint: wrong, key: key, command: command).open(.control)
            XCTFail("a changed host key was accepted")
        } catch is HostKeyChanged {}

        // One call on its own connection, as a notification action does.
        let t = NIOSSHTransport(user: user, endpoints: [ep], hostKeyFingerprint: SSHFingerprint.of(probed), key: key, command: command)
        let one = try await HabOneShot.call(t, HabMethods.habPing, PingParams(), clientLabel: "live-check")
        print("live-nio: one-shot ping (connect, hello, ping) \(ms(one.elapsed))")

        // The control connection: hello, subscribe, ping.
        let before = OpenStreams.live
        let clock = ContinuousClock()
        let start = clock.now
        let conn = HabConnection(id: "live-nio", transport: t, clientLabel: "live-check")
        await conn.start()
        await eventually("online", timeout: 20) { await conn.state.status == .online }
        let online = clock.now - start
        let st = await conn.state
        XCTAssertEqual(st.status, .online, st.lastError ?? "")
        let w = try XCTUnwrap(st.welcome)
        _ = try await conn.call(HabMethods.habPing, PingParams())
        print("live-nio: online (connect, hello, subscribe) in \(ms(online)); revision \(w.revision), sessions \(st.store.sessions.count), device_id \(w.deviceId ?? "-")")

        // A data channel on the same SSH connection, with a live pane.
        if let script = env["FLIGHTDECK_LIVE_NIO_PANE"], !script.isEmpty {
            let made = try await conn.call(HabMethods.sessionCreate, SessionCreateParams(argv: ["/bin/sh", "-c", script], cols: 80, name: "nio-live", rows: 24))
            let ps = PaneStream(pane: made.pane, connection: conn, observe: true)
            let got = LiveBytes()
            let t0 = clock.now
            await ps.start { await got.add($0) }
            await eventually("pane output over the data channel", timeout: 15) { await got.text.contains("flightdeck-nio-live") }
            print("live-nio: data channel, pane output in \(ms(clock.now - t0))")
            let n = await t.connectionCount()
            XCTAssertEqual(n, 1, "the data channel shares the control connection")
            await ps.close()
            _ = try? await conn.call(HabMethods.sessionClose, SessionRef(session: made.session))
        }

        await conn.stop()
        await eventually("closed", timeout: 10) { OpenStreams.live == before }
        await eventually("ssh connection closed", timeout: 10) { await t.connectionCount() == 0 }
    }

    /// An imported passphrase-protected key reaches a real sshd that trusts
    /// it, with no pairing step (app.md 10.4).
    func testLiveImportedKey() async throws {
        guard let portText = env["FLIGHTDECK_LIVE_NIO_PORT"], let port = Int(portText),
              let keyPath = env["FLIGHTDECK_LIVE_NIO_IMPORT_KEY"], let pass = env["FLIGHTDECK_LIVE_NIO_IMPORT_PASS"] else {
            throw XCTSkip("FLIGHTDECK_LIVE_NIO_PORT or FLIGHTDECK_LIVE_NIO_IMPORT_KEY not set")
        }
        let host = env["FLIGHTDECK_LIVE_NIO_HOST"] ?? "127.0.0.1"
        let user = env["FLIGHTDECK_LIVE_NIO_USER"] ?? env["USER"] ?? ""
        let command = try XCTUnwrap(env["FLIGHTDECK_LIVE_NIO_COMMAND"])
        let hostKeyLine = try String(contentsOfFile: try XCTUnwrap(env["FLIGHTDECK_LIVE_NIO_HOSTKEY"]), encoding: .utf8)
        let hostKey = try NIOSSHPublicKey(openSSHPublicKey: hostKeyLine.split(separator: " ").prefix(2).joined(separator: " "))

        let file = try OpenSSHKeyFile.parse(Data(contentsOf: URL(fileURLWithPath: keyPath)))
        XCTAssertTrue(file.isEncrypted)
        XCTAssertThrowsError(try file.decrypt(passphrase: "wrong")) { XCTAssertEqual($0 as? KeyImportError, .wrongPassphrase) }
        let keys = DeviceKeys(store: MemorySecretStore(), preferEnclave: false)
        let k = try keys.importKey(try file.decrypt(passphrase: pass)).key
        print("live-nio: imported \(k.type.label) key \(k.fingerprint)")

        let entry = SSHHabEntry(user: user, endpoints: [SSHEndpoint(host: host, port: port)], hostKeyFingerprint: SSHFingerprint.of(hostKey), keyID: k.id, command: command)
        let one = try await HabOneShot.call(try entry.transport(keys: keys), HabMethods.habPing, PingParams(), clientLabel: "live-check")
        print("live-nio: imported key one-shot ping (connect, hello, ping) \(ms(one.elapsed))")
    }

    private func appendLine(_ line: String, to path: String) throws {
        if !FileManager.default.fileExists(atPath: path) {
            FileManager.default.createFile(atPath: path, contents: nil, attributes: [.posixPermissions: 0o600])
        }
        let h = try FileHandle(forWritingTo: URL(fileURLWithPath: path))
        defer { try? h.close() }
        try h.seekToEnd()
        try h.write(contentsOf: Data((line + "\n").utf8))
    }

    private func ms(_ d: Duration) -> String {
        "\(d.components.seconds * 1000 + d.components.attoseconds / 1_000_000_000_000_000) ms"
    }
}

private actor LiveBytes {
    var data = Data()
    var text: String { String(decoding: data, as: UTF8.self) }
    func add(_ e: PaneStreamEvent) {
        switch e {
        case .snapshot(_, _, let d): data.append(d)
        case .bytes(_, let d): data.append(d)
        default: break
        }
    }
}
