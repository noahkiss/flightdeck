import Foundation
import HabAPI
import NIOSSH
import XCTest
@testable import FlightdeckKit

/// The pairing offer parser, endpoints, fingerprints, device keys and the
/// hab entry file.
final class SSHPairingTests: XCTestCase {
    // Public keys made for this test with ssh-keygen; `ssh-keygen -l` printed the fingerprints.
    let ecdsaLine = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBJJwhFEdoOmRlD9f+vf3FS8snTgEdpwHq1uvYakLjb0sAfghlM/3WZ3JWvWBqBPMFI9LhAd2A5dJV6rN0hnH5mc= test"
    let ecdsaFP = "SHA256:KD7JH4qKAq1d/l1LYWncuUzy/YMDVfY3DcqsOU60uhU"
    let ed25519Line = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH3wcR55pMSnXlY30uLyzEG+wh3hqL8SBYKrDsFWoDgs test"
    let ed25519FP = "SHA256:I5fSeR86Vlr+bQlX424FTWj5ZLBJpJ6n8ydcMbKHC9o"

    let token = Data((0..<16).map { UInt8($0 * 7 & 0xff) })

    // MARK: Fingerprints

    func testFingerprintMatchesSSHKeygen() throws {
        XCTAssertEqual(SSHFingerprint.of(try NIOSSHPublicKey(openSSHPublicKey: "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBJJwhFEdoOmRlD9f+vf3FS8snTgEdpwHq1uvYakLjb0sAfghlM/3WZ3JWvWBqBPMFI9LhAd2A5dJV6rN0hnH5mc=")), ecdsaFP)
        XCTAssertEqual(SSHFingerprint.of(try NIOSSHPublicKey(openSSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIH3wcR55pMSnXlY30uLyzEG+wh3hqL8SBYKrDsFWoDgs")), ed25519FP)
        XCTAssertTrue(SSHFingerprint.isValid(ecdsaFP))
        XCTAssertFalse(SSHFingerprint.isValid("SHA256:short"))
        XCTAssertFalse(SSHFingerprint.isValid("MD5:" + String(repeating: "a", count: 43)))
    }

    // MARK: Device keys

    func testDeviceKeyIsMadeOnceAndReloads() throws {
        let store = MemorySecretStore()
        let a = try DeviceKeys(store: store, preferEnclave: false).defaultKey()
        let b = try DeviceKeys(store: store, preferEnclave: false).defaultKey()
        XCTAssertEqual(a.fingerprint, b.fingerprint)
        XCTAssertEqual(a.kind, .software)
        XCTAssertFalse(a.requiresBiometry)
        let line = a.authorizedKeysLine(comment: "flightdeck-test")
        XCTAssertTrue(line.hasPrefix("ecdsa-sha2-nistp256 "))
        XCTAssertTrue(line.hasSuffix(" flightdeck-test"))
        // The line parses back to the same key.
        let parsed = try NIOSSHPublicKey(openSSHPublicKey: line.split(separator: " ").prefix(2).joined(separator: " "))
        XCTAssertEqual(parsed, a.publicKey)
    }

    func testReplacingTheKeyChangesTheFingerprint() throws {
        let keys = DeviceKeys(store: MemorySecretStore(), preferEnclave: false)
        let a = try keys.defaultKey()
        let b = try keys.create(id: DeviceKeys.defaultID)
        XCTAssertNotEqual(a.fingerprint, b.fingerprint)
        XCTAssertEqual(try keys.defaultKey().fingerprint, b.fingerprint)
        try keys.delete(DeviceKeys.defaultID)
        XCTAssertNil(try keys.key(DeviceKeys.defaultID))
    }

    func testEnclaveKeyWhenAvailable() throws {
        guard DeviceKeys.enclaveAvailable else { throw XCTSkip("no Secure Enclave") }
        let k: GeneratedKey
        do {
            k = try DeviceKeys(store: MemorySecretStore()).defaultKey()
        } catch let e as NSError where e.code == Int(errSecInteractionNotAllowed) {
            // The key is usable only while the device is unlocked, as designed.
            throw XCTSkip("the screen is locked: \(e.localizedDescription)")
        }
        XCTAssertEqual(k.kind, .secureEnclave)
        _ = try k.privateKey()
    }

    // MARK: Endpoints

    func testEndpointParsing() {
        XCTAssertEqual(SSHEndpoint.parse("box"), SSHEndpoint(host: "box", port: 22))
        XCTAssertEqual(SSHEndpoint.parse("box:2222"), SSHEndpoint(host: "box", port: 2222))
        XCTAssertEqual(SSHEndpoint.parse("192.0.2.4:22"), SSHEndpoint(host: "192.0.2.4", port: 22))
        XCTAssertEqual(SSHEndpoint.parse("[2001:db8::1]:2200"), SSHEndpoint(host: "2001:db8::1", port: 2200))
        XCTAssertEqual(SSHEndpoint.parse("[2001:db8::1]"), SSHEndpoint(host: "2001:db8::1", port: 22))
        XCTAssertEqual(SSHEndpoint.parse("2001:db8::1"), SSHEndpoint(host: "2001:db8::1", port: 22))
        XCTAssertEqual(SSHEndpoint.parse("[fe80::1%en0]:22"), SSHEndpoint(host: "fe80::1%en0", port: 22))
        XCTAssertNil(SSHEndpoint.parse("a:b:c"))
        XCTAssertNil(SSHEndpoint.parse("host:22:33"))
        XCTAssertNil(SSHEndpoint.parse("[box]:22"))
        XCTAssertNil(SSHEndpoint.parse("[2001:db8::zz]"))
        XCTAssertNil(SSHEndpoint.parse(""))
        XCTAssertNil(SSHEndpoint.parse("box:0"))
        XCTAssertNil(SSHEndpoint.parse("box:99999"))
        XCTAssertNil(SSHEndpoint.parse("box:ssh"))
        XCTAssertNil(SSHEndpoint.parse("a b"))
        XCTAssertNil(SSHEndpoint.parse(":22"))
        XCTAssertEqual(SSHEndpoint(host: "2001:db8::1", port: 2200).description, "[2001:db8::1]:2200")
        XCTAssertEqual(SSHEndpoint(host: "box").description, "box")
    }

    func testDestinationParsing() {
        XCTAssertEqual(SSHDestination.parse("alice@box:2222"), SSHDestination(user: "alice", endpoint: SSHEndpoint(host: "box", port: 2222)))
        XCTAssertEqual(SSHDestination.parse(" alice@box "), SSHDestination(user: "alice", endpoint: SSHEndpoint(host: "box")))
        XCTAssertEqual(SSHDestination.parse("alice@[2001:db8::1]:22")?.endpoint.host, "2001:db8::1")
        XCTAssertNil(SSHDestination.parse("box"))
        XCTAssertNil(SSHDestination.parse("@box"))
        XCTAssertNil(SSHDestination.parse("-oProxyCommand@box"))
        XCTAssertNil(SSHDestination.parse("alice@"))
    }

    // MARK: Pairing offer

    private var tokenText: String { PairingCode.base64url(token) }

    func testOfferRoundTrip() throws {
        let o = PairingCode(deviceID: ecdsaFP, endpoints: [SSHEndpoint(host: "192.0.2.10", port: 2222), SSHEndpoint(host: "hab.example"), SSHEndpoint(host: "2001:db8::5", port: 2222)], token: token, label: "desk")
        let parsed = try PairingCode.parse(o.link)
        XCTAssertEqual(parsed, o)
        XCTAssertEqual(tokenText.count, 22)
    }

    func testOfferFromAPercentEncodedLink() throws {
        let fp = ecdsaFP.addingPercentEncoding(withAllowedCharacters: .alphanumerics)!
        let link = "flightdeck://pair?v=1&id=\(fp)&ep=192.0.2.10%3A2222&ep=%5B2001%3Adb8%3A%3A5%5D%3A2222&t=\(tokenText)&l=build%20box"
        let o = try PairingCode.parse(link)
        XCTAssertEqual(o.deviceID, ecdsaFP)
        XCTAssertEqual(o.endpoints, [SSHEndpoint(host: "192.0.2.10", port: 2222), SSHEndpoint(host: "2001:db8::5", port: 2222)])
        XCTAssertEqual(o.token, token)
        XCTAssertEqual(o.label, "build box")
    }

    func testOfferInsidePastedText() throws {
        let text = "Scan this, or paste it into the app:\n  flightdeck://pair?v=1&id=\(ecdsaFP)&ep=hab.example:2222&t=\(tokenText)\nIt expires in 5 minutes."
        let o = try PairingCode.parse(text)
        XCTAssertEqual(o.endpoints, [SSHEndpoint(host: "hab.example", port: 2222)])
        XCTAssertNil(o.label)
    }

    func testOfferIgnoresUnknownKeysAndKeepsEndpointOrder() throws {
        let o = try PairingCode.parse("FLIGHTDECK://pair?v=1&x=later&id=\(ecdsaFP)&ep=b:2&ep=a:1&ep=b:2&t=\(tokenText)")
        XCTAssertEqual(o.endpoints, [SSHEndpoint(host: "b", port: 2), SSHEndpoint(host: "a", port: 1)])
    }

    func testOfferErrors() {
        func err(_ s: String) -> PairingCodeError? {
            do { _ = try PairingCode.parse(s); return nil } catch { return error as? PairingCodeError }
        }
        let ok = "id=\(ecdsaFP)&ep=hab.example:2222&t=\(tokenText)"
        XCTAssertEqual(err("hello"), .notAnOffer)
        XCTAssertEqual(err("https://example.com/pair?v=1&\(ok)"), .notAnOffer)
        XCTAssertEqual(err("flightdeck://open?v=1&\(ok)"), .notAnOffer)
        XCTAssertEqual(err("flightdeck://pair?\(ok)"), .missing("v"))
        XCTAssertEqual(err("flightdeck://pair?v=2&\(ok)"), .unsupportedVersion("2"))
        XCTAssertEqual(err("flightdeck://pair?v=1&ep=a&t=\(tokenText)"), .missing("id"))
        XCTAssertEqual(err("flightdeck://pair?v=1&id=SHA256:nope&ep=a&t=\(tokenText)"), .badDeviceID("SHA256:nope"))
        XCTAssertEqual(err("flightdeck://pair?v=1&id=\(ecdsaFP)&t=\(tokenText)"), .missing("ep"))
        XCTAssertEqual(err("flightdeck://pair?v=1&id=\(ecdsaFP)&ep=a:70000&t=\(tokenText)"), .badEndpoint("a:70000"))
        XCTAssertEqual(err("flightdeck://pair?v=1&id=\(ecdsaFP)&ep=a"), .missing("t"))
        XCTAssertEqual(err("flightdeck://pair?v=1&id=\(ecdsaFP)&ep=a&t=AAAA"), .badToken)
        XCTAssertEqual(err("flightdeck://pair?v=1&id=\(ecdsaFP)&ep=a&t=\(tokenText)!"), .badToken)
        XCTAssertFalse(PairingCodeError.badToken.description.isEmpty)
    }

    func testOfferWithARepeatedKeyIsRefused() {
        func err(_ s: String) -> PairingCodeError? {
            do { _ = try PairingCode.parse(s); return nil } catch { return error as? PairingCodeError }
        }
        let other = PairingCode.base64url(Data(repeating: 7, count: 16))
        XCTAssertEqual(err("flightdeck://pair?v=1&v=1&id=\(ecdsaFP)&ep=a&t=\(tokenText)"), .repeated("v"))
        XCTAssertEqual(err("flightdeck://pair?v=1&id=\(ecdsaFP)&id=\(ecdsaFP)&ep=a&t=\(tokenText)"), .repeated("id"))
        XCTAssertEqual(err("flightdeck://pair?v=1&id=\(ecdsaFP)&ep=a&t=\(tokenText)&t=\(other)"), .repeated("t"))
        XCTAssertEqual(err("flightdeck://pair?v=1&id=\(ecdsaFP)&ep=a&t=\(tokenText)&l=a&l=b"), .repeated("l"))
    }

    func testLabelLosesControlAndFormatCharacters() throws {
        let o = try PairingCode.parse("flightdeck://pair?v=1&id=\(ecdsaFP)&ep=a&t=\(tokenText)&l=de%E2%80%AEsk%07")
        XCTAssertEqual(o.label, "desk")
    }

    func testOfferEndpointCap() throws {
        func link(_ n: Int) -> String {
            let eps = (1...n).map { "ep=192.0.2.\($0):22" }.joined(separator: "&")
            return "flightdeck://pair?v=1&id=\(ecdsaFP)&\(eps)&t=\(tokenText)"
        }
        XCTAssertEqual(try PairingCode.parse(link(8)).endpoints.count, 8)
        XCTAssertThrowsError(try PairingCode.parse(link(9))) { e in
            XCTAssertEqual(e as? PairingCodeError, .tooManyEndpoints(9))
        }
    }

    // MARK: Hab entries

    func testHabListRoundTrip() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        let url = dir.appendingPathComponent("ssh-habs.json")
        XCTAssertEqual(try SSHHabList.load(from: url), SSHHabList())
        let e = SSHHabEntry(label: "desk", user: "alice", endpoints: [SSHEndpoint(host: "hab.example", port: 2222)], hostKeyFingerprint: ed25519FP, hostKey: ed25519Line, deviceID: ecdsaFP, added: Date(timeIntervalSince1970: 1_800_000_000))
        try SSHHabList(habs: [e]).save(to: url)
        XCTAssertEqual(try SSHHabList.load(from: url), SSHHabList(habs: [e]))
        let mode = try FileManager.default.attributesOfItem(atPath: url.path)[.posixPermissions] as? Int
        XCTAssertEqual(mode, 0o600)
        XCTAssertEqual(e.destination, "alice@hab.example:2222")
    }

    func testEntryWithAMissingKeySaysSo() {
        let e = SSHHabEntry(user: "alice", endpoints: [SSHEndpoint(host: "h")], hostKeyFingerprint: ed25519FP, keyID: "gone")
        XCTAssertThrowsError(try e.transport(keys: DeviceKeys(store: MemorySecretStore(), preferEnclave: false))) { err in
            XCTAssertTrue("\(err)".contains("no key"))
        }
    }
}

extension SSHPairingTests {
    func testCodeFromTheAPIOffer() throws {
        let api = PairingOffer(deviceId: ecdsaFP, endpoints: ["192.0.2.10:2222", "[2001:db8::5]:2222"], expiresInMs: 300_000, seq: 1, token: PairingCode.base64url(token))
        let code = try XCTUnwrap(PairingCode(offer: api, label: "desk"))
        XCTAssertEqual(try PairingCode.parse(code.link), code)
        XCTAssertEqual(code.endpoints.count, 2)
        var bad = api
        bad.token = "short"
        XCTAssertNil(PairingCode(offer: bad))
    }
}
