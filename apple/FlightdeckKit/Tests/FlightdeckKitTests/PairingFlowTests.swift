// Process and the real /bin/sh exist only on macOS; the iOS Simulator run skips these.
#if os(macOS)
import Foundation
import NIOSSH
import XCTest
@testable import FlightdeckKit

/// The app's half of QR pairing (transport.md 6.1.2, app.md 18.8) and the
/// password setup (transport.md 6.1.6).
final class PairingFlowTests: XCTestCase {
    let token = Data((0..<16).map { UInt8($0 * 7 + 3) })

    func code(_ s: TestPairServer, label: String? = "box") -> PairingCode {
        PairingCode(deviceID: s.hostKeyFingerprint, endpoints: [s.endpoint], token: token, label: label)
    }

    func fastPairing(owner: Duration = .seconds(5)) -> Pairing.Timing {
        var t = Pairing.Timing()
        t.ssh = fastSSH()
        t.firstAnswer = .seconds(3)
        t.owner = owner
        return t
    }

    static let edKey = "ssh-ed25519 " + String(openSSHPublicKey: NIOSSHPrivateKey(ed25519Key: .init()).publicKey).split(separator: " ")[1]
    static let ecKey = String(openSSHPublicKey: NIOSSHPrivateKey(p256Key: .init()).publicKey)

    func paired(_ s: TestPairServer, device: String, hostKeys: [String] = [edKey], endpoints: [String] = ["192.0.2.10:22", "198.51.100.7"], user: String = "owner") -> String {
        let obj: [String: Any] = [
            "v": 1, "state": "paired", "hab": ["id": s.hostKeyFingerprint, "label": "box"], "device_id": device,
            "user": user, "ssh": ["endpoints": endpoints, "host_keys": hostKeys], "forced_command": true,
        ]
        return String(decoding: try! JSONSerialization.data(withJSONObject: obj), as: UTF8.self)
    }

    // MARK: - Codec

    func testRequestLine() throws {
        let line = Pairing.requestLine(token: token, name: "  Owner's\u{202E} \t iPhone\n", kind: .tablet)
        XCTAssertEqual(line.last, 0x0A)
        let obj = try JSONSerialization.jsonObject(with: line.dropLast()) as! [String: Any]
        XCTAssertEqual(obj["v"] as? Int, 1)
        XCTAssertEqual(obj["token"] as? String, PairingCode.base64url(token))
        XCTAssertEqual((obj["token"] as? String)?.count, 22)
        XCTAssertEqual(obj["name"] as? String, "Owner's iPhone")
        XCTAssertEqual(obj["kind"] as? String, "tablet")
        XCTAssertFalse(line.dropLast().contains(0x0A))
    }

    func testCleanName() {
        XCTAssertEqual(Pairing.cleanName("\u{0007}\u{200F}"), "Flightdeck device")
        XCTAssertEqual(Pairing.cleanName(String(repeating: "a", count: 100)).count, 64)
        XCTAssertEqual(Pairing.cleanName(" a  b "), "a b")
    }

    func testConfirmCode() {
        XCTAssertEqual(Pairing.confirmCode("SHA256:abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPq"), "NOPq")
    }

    func testAnswerParsing() throws {
        XCTAssertEqual(try Pairing.Answer.parse(Data(#"{"v":1,"state":"waiting"}"#.utf8)), .waiting)
        XCTAssertEqual(try Pairing.Answer.parse(Data(#"{"v":1,"state":"refused","reason":"token"}"#.utf8)), .refused("token"))
        for bad in [
            "", "not json", "[]", #"{"state":"waiting"}"#, #"{"v":2,"state":"waiting"}"#, #"{"v":"1","state":"waiting"}"#,
            #"{"v":1,"state":"maybe"}"#, #"{"v":1}"#, #"{"v":1,"state":"paired"}"#,
            #"{"v":1,"state":"paired","hab":{"id":"x"},"device_id":"y","user":"u"}"#,
        ] {
            XCTAssertThrowsError(try Pairing.Answer.parse(Data(bad.utf8)), bad) { XCTAssertTrue($0 is PairingFailed, bad) }
        }
        let long = Data(repeating: 0x20, count: Pairing.maxAnswer + 1)
        XCTAssertThrowsError(try Pairing.Answer.parse(long))
    }

    func testRefusalWords() {
        XCTAssertEqual(PairingRefused(reason: "token").description, "This code is not valid on this hab.")
        XCTAssertEqual(PairingRefused(reason: "expired").description, "The code expired: run gff-hab pair again.")
        XCTAssertEqual(PairingRefused(reason: "declined").description, "Declined on the hab.")
        XCTAssertEqual(PairingRefused(reason: "busy").description, "Another device is pairing: try again.")
        XCTAssertEqual(PairingRefused(reason: "version").description, "Update Flightdeck.")
        XCTAssertEqual(PairingRefused(reason: "failed").description, "The hab could not save the key: see its terminal.")
        XCTAssertEqual(PairingRefused(reason: "request").description, "Pairing failed.")
        XCTAssertEqual(PairingRefused(reason: "\u{1B}[2J").description, "Pairing failed.")
    }

    // MARK: - The paired answer

    func answer(_ json: String) throws -> Pairing.Paired {
        guard case .paired(let p) = try Pairing.Answer.parse(Data(json.utf8)) else { throw PairingFailed("not paired") }
        return p
    }

    func testEntryFromAnswer() throws {
        let s = TestPairServer()
        let c = code(s, label: nil)
        let p = try answer(paired(s, device: "SHA256:dev", hostKeys: ["ssh-rsa AAAAB3Nza", "garbage", Self.ecKey, Self.edKey + " root@box"]))
        let r = try Pairing.entry(from: p, code: c, deviceFingerprint: "SHA256:dev", keyID: "device")
        XCTAssertFalse(r.needsHostKey)
        XCTAssertEqual(r.entry.user, "owner")
        XCTAssertEqual(r.entry.endpoints, [SSHEndpoint(host: "192.0.2.10"), SSHEndpoint(host: "198.51.100.7")])
        // The ed25519 key wins over the ECDSA key listed first.
        XCTAssertEqual(r.entry.hostKey, Self.edKey)
        XCTAssertEqual(r.entry.hostKeyFingerprint, SSHFingerprint.of(try NIOSSHPublicKey(openSSHPublicKey: Self.edKey)))
        XCTAssertEqual(r.entry.deviceID, s.hostKeyFingerprint)
        XCTAssertEqual(r.entry.label, "box")
        XCTAssertNil(r.entry.command)

        let ec = try Pairing.entry(from: try answer(paired(s, device: "SHA256:dev", hostKeys: [Self.ecKey])), code: code(s), deviceFingerprint: "SHA256:dev", keyID: "device")
        XCTAssertEqual(ec.entry.hostKey, Self.ecKey)

        let none = try Pairing.entry(from: try answer(paired(s, device: "SHA256:dev", hostKeys: ["ssh-rsa AAAA"])), code: code(s), deviceFingerprint: "SHA256:dev", keyID: "device")
        XCTAssertTrue(none.needsHostKey)
        XCTAssertNil(none.entry.hostKey)
    }

    @MainActor
    func testUnfinishedPairingSurvivesSaveFailure() async throws {
        let s = TestPairServer()
        let entry = try Pairing.entry(from: answer(paired(s, device: "SHA256:dev")), code: code(s), deviceFingerprint: "SHA256:dev", keyID: "device").entry
        let setup = SSHSetup(keys: DeviceKeys(store: MemorySecretStore(), preferEnclave: false), listURL: nil, fleet: nil)
        do {
            try await setup.finishPairing(entry)
            XCTFail("save should fail")
        } catch {
            XCTAssertEqual(setup.unfinishedPairing, entry)
        }
    }

    @MainActor
    func testPairingProbeTriesOtherEndpoints() async throws {
        let server = TestPairServer()
        try await server.start()
        defer { server.stop() }
        let setup = SSHSetup(keys: DeviceKeys(store: MemorySecretStore(), preferEnclave: false), listURL: nil, fleet: nil, timing: fastSSH())
        var entry = try Pairing.entry(from: answer(paired(server, device: "SHA256:dev", hostKeys: [])), code: code(server), deviceFingerprint: "SHA256:dev", keyID: DeviceKeys.defaultID).entry
        entry.endpoints = [SSHEndpoint(host: "127.0.0.1", port: 1), server.endpoint]
        let (endpoint, key) = try await setup.probePairing(entry)
        XCTAssertEqual(endpoint, server.endpoint)
        XCTAssertEqual(SSHFingerprint.of(key), server.hostKeyFingerprint)
    }

    func testEntryRefusals() throws {
        let s = TestPairServer()
        let c = code(s)
        func refuses(_ json: String, device: String = "SHA256:dev", _ why: String, line: UInt = #line) {
            XCTAssertThrowsError(try Pairing.entry(from: try answer(json), code: c, deviceFingerprint: device, keyID: "device"), why, line: line)
        }
        refuses(paired(s, device: "SHA256:other"), "another key admitted")
        refuses(paired(TestPairServer(), device: "SHA256:dev"), "another hab id")
        for u in ["", "-oProxyCommand=x", "a b", "a@b", "a\"b", String(repeating: "u", count: 65), "üser", "a\u{0}"] {
            refuses(paired(s, device: "SHA256:dev", user: u), "user \(u)")
        }
        refuses(paired(s, device: "SHA256:dev", endpoints: []), "no endpoints")
        refuses(paired(s, device: "SHA256:dev", endpoints: ["a b"]), "bad endpoint")
        refuses(paired(s, device: "SHA256:dev", endpoints: ["host:0"]), "bad port")
        refuses(paired(s, device: "SHA256:dev", endpoints: (1...9).map { "h\($0)" }), "too many")
    }

    // MARK: - Against a fake window

    func testPairsWithTheWindow() async throws {
        let key = try testKey()
        let s = TestPairServer()
        let fp = key.fingerprint
        let answer = paired(s, device: fp)
        s.pairHandler = { _, write, close in
            write(#"{"v":1,"state":"waiting"}"#)
            DispatchQueue.global().asyncAfter(deadline: .now() + 0.2) {
                write(answer)
                close()
            }
        }
        try await s.start()
        defer { s.stop() }
        let shown = Box<String>()
        let r = try await Pairing.pair(code(s), key: key, keyID: "device", name: "Test phone", kind: .phone, timing: fastPairing()) { shown.set($0) }
        XCTAssertEqual(shown.get(), fp)
        XCTAssertEqual(Pairing.confirmCode(shown.get()!), String(fp.suffix(4)))
        XCTAssertEqual(r.entry.user, "owner")
        XCTAssertEqual(s.subsystems, ["flightdeck-pair"])
        XCTAssertEqual(s.execs, [])
        XCTAssertEqual(s.users, [Pairing.user])
        XCTAssertEqual(s.keys, [key.publicKey])
        let req = try JSONSerialization.jsonObject(with: s.requests[0]) as! [String: Any]
        XCTAssertEqual(req["token"] as? String, PairingCode.base64url(token))
        XCTAssertEqual(req["name"] as? String, "Test phone")
        XCTAssertEqual(req["kind"] as? String, "phone")
    }

    func testWrongHostKeyStopsBeforeAuth() async throws {
        let key = try testKey()
        let s = TestPairServer()
        s.pairHandler = { _, write, _ in write(#"{"v":1,"state":"waiting"}"#) }
        try await s.start()
        defer { s.stop() }
        var c = code(s)
        c.deviceID = SSHFingerprint.of(NIOSSHPrivateKey(ed25519Key: .init()).publicKey)
        do {
            _ = try await Pairing.pair(c, key: key, keyID: "device", name: "x", kind: .phone, timing: fastPairing()) { _ in }
            XCTFail("paired with the wrong host")
        } catch let e as PairingHostKeyMismatch {
            XCTAssertEqual(e.presented, s.hostKeyFingerprint)
        }
        XCTAssertEqual(s.keys, [], "no key was offered to the wrong host")
        XCTAssertEqual(s.requests, [], "the token never reached the wrong host")
    }

    func testRefusedAnswers() async throws {
        let key = try testKey()
        for reason in ["token", "expired", "declined", "busy", "version", "failed", "request"] {
            let s = TestPairServer()
            s.pairHandler = { _, write, close in
                write(#"{"v":1,"state":"refused","reason":"\#(reason)"}"#)
                close()
            }
            try await s.start()
            do {
                _ = try await Pairing.pair(code(s), key: key, keyID: "device", name: "x", kind: .phone, timing: fastPairing()) { _ in XCTFail("waiting shown") }
                XCTFail("no refusal")
            } catch let e as PairingRefused {
                XCTAssertEqual(e.reason, reason)
            }
            s.stop()
        }
    }

    func testDeclinedAfterWaiting() async throws {
        let key = try testKey()
        let s = TestPairServer()
        s.pairHandler = { _, write, close in
            write(#"{"v":1,"state":"waiting"}"#)
            write(#"{"v":1,"state":"refused","reason":"declined"}"#)
            close()
        }
        try await s.start()
        defer { s.stop() }
        let shown = Box<String>()
        await XCTAssertThrowsAsync(try await Pairing.pair(code(s), key: key, keyID: "device", name: "x", kind: .phone, timing: fastPairing()) { shown.set($0) }) {
            XCTAssertEqual($0 as? PairingRefused, PairingRefused(reason: "declined"))
        }
        XCTAssertNotNil(shown.get())
    }

    func testBadAndMissingAnswers() async throws {
        let key = try testKey()
        let cases: [(String, TestPairServer.PairHandler)] = [
            ("closed", { _, _, close in close() }),
            ("garbage", { _, write, close in write("hello"); close() }),
            ("half", { _, write, close in write(#"{"v":1,"state":"waiting"}"#); close() }),
            ("twice", { _, write, close in write(#"{"v":1,"state":"waiting"}"#); write(#"{"v":1,"state":"waiting"}"#); close() }),
            ("silent owner", { _, write, _ in write(#"{"v":1,"state":"waiting"}"#) }),
            ("flood", { _, write, _ in write(String(repeating: "x", count: Pairing.maxAnswer + 10)) }),
        ]
        for (name, h) in cases {
            let s = TestPairServer()
            s.pairHandler = h
            try await s.start()
            await XCTAssertThrowsAsync(try await Pairing.pair(code(s), key: key, keyID: "device", name: "x", kind: .phone, timing: fastPairing(owner: .milliseconds(500))) { _ in }) {
                XCTAssertTrue($0 is PairingFailed, "\(name): \($0)")
            }
            s.stop()
        }
    }

    func testStalledWindowTimesOut() async throws {
        let key = try testKey()
        let s = TestPairServer()
        s.silent = true
        s.pairHandler = { _, _, _ in }
        try await s.start()
        defer { s.stop() }
        var t = fastPairing()
        t.firstAnswer = .milliseconds(500)
        let start = Date()
        await XCTAssertThrowsAsync(try await Pairing.pair(code(s), key: key, keyID: "device", name: "x", kind: .phone, timing: t) { _ in }) {
            XCTAssertEqual($0 as? PairingFailed, PairingFailed("the hab did not answer in time"))
        }
        XCTAssertLessThan(Date().timeIntervalSince(start), 5)
    }

    func testEndpointsRefuseFormatCharacters() {
        XCTAssertNil(SSHEndpoint.parse("box\u{202E}.lan"))
        XCTAssertNil(SSHEndpoint.parse("box\u{0007}"))
        XCTAssertNotNil(SSHEndpoint.parse("box.lan:22"))
    }

    func testCancelClosesTheConnection() async throws {
        let key = try testKey()
        let s = TestPairServer()
        s.pairHandler = { _, write, _ in write(#"{"v":1,"state":"waiting"}"#) }
        try await s.start()
        defer { s.stop() }
        let shown = Box<String>()
        let t = Task { [self] in
            try await Pairing.pair(code(s), key: key, keyID: "device", name: "x", kind: .phone, timing: fastPairing(owner: .seconds(60))) { shown.set($0) }
        }
        let start = Date()
        while shown.get() == nil && Date().timeIntervalSince(start) < 5 { try await Task.sleep(for: .milliseconds(20)) }
        t.cancel()
        let r = await t.result
        XCTAssertThrowsError(try r.get())
        XCTAssertLessThan(Date().timeIntervalSince(start), 10)
    }

    // MARK: - Password setup

    func tempHome() throws -> URL {
        let u = FileManager.default.temporaryDirectory.appendingPathComponent("fd-pw-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: u, withIntermediateDirectories: true)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: u.path)
        addTeardownBlock { try? FileManager.default.removeItem(at: u) }
        return u
    }

    func runScript(home: URL, line: String, key: NIOSSHPublicKey, path: String = "/usr/bin:/bin") throws -> PasswordSetup.Outcome {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/bin/sh")
        p.arguments = ["-s"]
        p.environment = ["HOME": home.path, "PATH": path]
        let i = Pipe(), o = Pipe()
        p.standardInput = i
        p.standardOutput = o
        try p.run()
        try i.fileHandleForWriting.write(contentsOf: Data(PasswordSetup.script(line: line, key: key).utf8))
        try i.fileHandleForWriting.close()
        let out = o.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        return try PasswordSetup.outcome(out)
    }

    func testKeyLineAndCommand() throws {
        let key = try testKey()
        let day = Date(timeIntervalSince1970: 1_793_620_800) // 2026-11-02
        let line = PasswordSetup.keyLine(publicKey: key.publicKey, command: try PasswordSetup.forcedCommand(nil), name: "Owner's\n iPhone", date: day)
        XCTAssertEqual(line, "restrict,command=\"gff-hab connect\" \(String(openSSHPublicKey: key.publicKey)) flightdeck: Owners iPhone (paired 2026-11-02)")
        XCTAssertEqual(try PasswordSetup.forcedCommand("  /opt/bin/gff-hab connect "), "/opt/bin/gff-hab connect")
        for bad in ["a\"b", "a'b", "a\\b", "a\nb", "a\u{202E}b", String(repeating: "c", count: 513)] {
            XCTAssertThrowsError(try PasswordSetup.forcedCommand(bad), bad)
        }
    }

    func testScriptAppendsAndKeepsOtherLines() throws {
        let key = try testKey()
        let home = try tempHome()
        let line = PasswordSetup.keyLine(publicKey: key.publicKey, command: "gff-hab connect", name: "phone")
        // No ~/.ssh yet: made at 0700, the file at 0600.
        XCTAssertEqual(try runScript(home: home, line: line, key: key.publicKey), .added)
        let ssh = home.appendingPathComponent(".ssh")
        let f = ssh.appendingPathComponent("authorized_keys")
        XCTAssertEqual(try String(contentsOf: f, encoding: .utf8), line + "\n")
        XCTAssertEqual((try FileManager.default.attributesOfItem(atPath: ssh.path)[.posixPermissions] as? NSNumber)?.intValue, 0o700)
        XCTAssertEqual((try FileManager.default.attributesOfItem(atPath: f.path)[.posixPermissions] as? NSNumber)?.intValue, 0o600)
        // Again: present, nothing written.
        XCTAssertEqual(try runScript(home: home, line: line, key: key.publicKey), .present)
        XCTAssertEqual(try String(contentsOf: f, encoding: .utf8), line + "\n")

        // Other lines byte for byte, CRLF and no final newline kept, one newline added first.
        let other = "# keep me\r\nssh-ed25519 AAAAother x\r\nno-final-newline"
        try Data(other.utf8).write(to: f)
        XCTAssertEqual(try runScript(home: home, line: line, key: key.publicKey), .added)
        XCTAssertEqual(try String(contentsOf: f, encoding: .utf8), other + "\n" + line + "\n")
    }

    func testScriptQuotesWhatItIsGiven() throws {
        let key = try testKey()
        let home = try tempHome()
        // A line with quotes and a break: written as one line, never run.
        let line = "x' ; touch \"$HOME/pwned\" ; echo '\nsecond"
        XCTAssertEqual(try runScript(home: home, line: line, key: key.publicKey), .added)
        XCTAssertFalse(FileManager.default.fileExists(atPath: home.appendingPathComponent("pwned").path))
        XCTAssertEqual(try String(contentsOf: home.appendingPathComponent(".ssh/authorized_keys"), encoding: .utf8), "x' ; touch \"$HOME/pwned\" ; echo 'second\n")
    }

    func testCommentedKeyIsNotPresent() throws {
        let key = try testKey()
        let home = try tempHome()
        let ssh = home.appendingPathComponent(".ssh")
        try FileManager.default.createDirectory(at: ssh, withIntermediateDirectories: true)
        let f = ssh.appendingPathComponent("authorized_keys")
        try Data("# \(String(openSSHPublicKey: key.publicKey))\n".utf8).write(to: f)
        let line = PasswordSetup.keyLine(publicKey: key.publicKey, command: "gff-hab connect", name: "phone")
        XCTAssertEqual(try runScript(home: home, line: line, key: key.publicKey), .added)
    }

    func testScriptFollowsALink() throws {
        let key = try testKey()
        let home = try tempHome()
        let ssh = home.appendingPathComponent(".ssh")
        try FileManager.default.createDirectory(at: ssh, withIntermediateDirectories: true)
        let real = home.appendingPathComponent("real_keys")
        try Data("a\n".utf8).write(to: real)
        try FileManager.default.createSymbolicLink(at: ssh.appendingPathComponent("authorized_keys"), withDestinationURL: real)
        let line = PasswordSetup.keyLine(publicKey: key.publicKey, command: "gff-hab connect", name: "phone")
        XCTAssertEqual(try runScript(home: home, line: line, key: key.publicKey), .added)
        XCTAssertEqual(try String(contentsOf: real, encoding: .utf8), "a\n" + line + "\n")
        XCTAssertEqual(try FileManager.default.destinationOfSymbolicLink(atPath: ssh.appendingPathComponent("authorized_keys").path), real.path)
    }

    func testScriptReportsChmodFailure() throws {
        let key = try testKey()
        let home = try tempHome()
        let bin = home.appendingPathComponent("bin")
        try FileManager.default.createDirectory(at: bin, withIntermediateDirectories: true)
        let chmod = bin.appendingPathComponent("chmod")
        try Data("#!/bin/sh\nexit 1\n".utf8).write(to: chmod)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: chmod.path)
        let line = PasswordSetup.keyLine(publicKey: key.publicKey, command: "gff-hab connect", name: "phone")
        XCTAssertThrowsError(try runScript(home: home, line: line, key: key.publicKey, path: bin.path + ":/usr/bin:/bin")) {
            XCTAssertTrue("\($0)".contains("could not secure"))
        }
    }

    func testScriptOutcomeRefusals() {
        XCTAssertThrowsError(try PasswordSetup.outcome(Data("Welcome\n".utf8)))
        XCTAssertThrowsError(try PasswordSetup.outcome(Data("flightdeck: error could not write the file\n".utf8))) {
            XCTAssertTrue("\($0)".contains("could not write"))
        }
    }

    func testPasswordSetupOverSSH() async throws {
        let key = try testKey()
        let s = TestPairServer(methods: .password)
        let pw = UUID().uuidString
        s.password = pw
        s.home = try tempHome()
        try await s.start()
        defer { s.stop() }
        let dest = SSHDestination(user: "owner", endpoint: s.endpoint)
        let line = PasswordSetup.keyLine(publicKey: key.publicKey, command: "gff-hab connect", name: "phone")

        // A host key that does not match: the password never leaves.
        let other = SSHFingerprint.of(NIOSSHPrivateKey(ed25519Key: .init()).publicKey)
        await XCTAssertThrowsAsync(try await PasswordSetup.install(dest, hostKeyFingerprint: other, password: pw, line: line, key: key.publicKey, timing: fastSSH())) {
            XCTAssertTrue($0 is HostKeyChanged, "\($0)")
        }
        XCTAssertEqual(s.passwords, [])

        // A wrong password is offered once.
        await XCTAssertThrowsAsync(try await PasswordSetup.install(dest, hostKeyFingerprint: s.hostKeyFingerprint, password: "x" + pw, line: line, key: key.publicKey, timing: fastSSH())) {
            XCTAssertTrue($0 is PasswordRefused, "\($0)")
        }
        XCTAssertEqual(s.passwords, ["x" + pw])

        let r = try await PasswordSetup.install(dest, hostKeyFingerprint: s.hostKeyFingerprint, password: pw, line: line, key: key.publicKey, timing: fastSSH())
        XCTAssertEqual(r, .added)
        XCTAssertEqual(s.execs, ["sh -s"])
        let f = s.home!.appendingPathComponent(".ssh/authorized_keys")
        XCTAssertEqual(try String(contentsOf: f, encoding: .utf8), line + "\n")
    }

    func testPasswordAuthOff() async throws {
        let key = try testKey()
        let s = TestPairServer(methods: .publicKey)
        try await s.start()
        defer { s.stop() }
        await XCTAssertThrowsAsync(try await PasswordSetup.install(SSHDestination(user: "owner", endpoint: s.endpoint), hostKeyFingerprint: s.hostKeyFingerprint, password: UUID().uuidString, line: "x", key: key.publicKey, timing: fastSSH())) {
            XCTAssertTrue($0 is PasswordAuthOff, "\($0)")
        }
    }
}

#endif
