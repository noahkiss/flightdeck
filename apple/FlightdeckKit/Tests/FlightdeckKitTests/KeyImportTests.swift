import CryptoKit
import Foundation
import HabAPI
import LocalAuthentication
import NIOSSH
import Security
import XCTest
@testable import FlightdeckKit

/// The OpenSSH key reader, bcrypt_pbkdf, the imported key store and an
/// imported key on the SSH carrier (app.md 10.4, 18.13). Key fixtures come
/// from `ssh-keygen` (`apple/scripts/gen-key-fixtures.sh`).
final class KeyImportTests: XCTestCase {
    private func hex(_ s: String) -> [UInt8] {
        var out: [UInt8] = []
        var i = s.startIndex
        while i < s.endIndex {
            let j = s.index(i, offsetBy: 2)
            out.append(UInt8(s[i..<j], radix: 16)!)
            i = j
        }
        return out
    }

    private func hexString(_ b: [UInt8]) -> String { b.map { String(format: "%02x", $0) }.joined() }

    // MARK: - Blowfish and bcrypt_pbkdf

    /// Eric Young's Blowfish vectors (the first two) and one from OpenSSL's
    /// bf-ecb with a 16-byte key. They prove the tables generated from pi.
    func testBlowfishKnownAnswers() {
        let cases: [(String, String, String)] = [
            ("0000000000000000", "0000000000000000", "4ef997456198dd78"),
            ("ffffffffffffffff", "ffffffffffffffff", "51866fd5b85ecb8a"),
            ("0123456789abcdeff0e1d2c3b4a59687", "0123456789abcdef", "c704ca5eeaace933"),
        ]
        for (k, p, c) in cases {
            let bf = Blowfish(key: hex(k))
            let pb = hex(p)
            var l = UInt32(pb[0]) << 24 | UInt32(pb[1]) << 16 | UInt32(pb[2]) << 8 | UInt32(pb[3])
            var r = UInt32(pb[4]) << 24 | UInt32(pb[5]) << 16 | UInt32(pb[6]) << 8 | UInt32(pb[7])
            bf.encrypt(&l, &r)
            XCTAssertEqual(String(format: "%08x%08x", l, r), c, "key \(k)")
        }
        XCTAssertEqual(BlowfishTables.p.count, 18)
        XCTAssertEqual([BlowfishTables.s0, BlowfishTables.s1, BlowfishTables.s2, BlowfishTables.s3].map(\.count), [256, 256, 256, 256])
    }

    /// Known answers from an independent implementation (pyca/bcrypt's
    /// `bcrypt.kdf`), covering one and several rounds, the 48 bytes
    /// aes256-ctr needs, and lengths that spread over several blocks.
    func testBcryptPBKDFKnownAnswers() throws {
        let cases: [(String, String, Int, Int, String)] = [
            ("70617373776f7264", "73616c74", 4, 32, "5bbf0cc293587f1c3635555c27796598d47e579071bf427e9d8fbe842aba34d9"),
            ("70617373776f7264", "73616c74", 1, 48, "7aecf4a3148069a4e52b28a060c4da05492ede72bf0caa4f6a2a4e5f1252a24ccb8a85077baa90b67b3a2bd1b2bb0d1c"),
            ("636f727265637420686f727365206261747465727920737461706c65", "000102030405060708090a0b0c0d0e0f", 16, 48, "800e37c007983f658e60a0bb3d6d9da43b1adf37371d89ce9a5506d6ed3efcf91f79d8b9d7617ea8f98bf45c362a3153"),
            ("00ff2070617373", "0102030405060708", 2, 88, "7654e1122e3c2c8b5b538d83b9abdcf272b410215d6c91c847673bacfed8658bfc0fbb7558a18e12a54f7bc1ed3b8b28952f4493d795bd540fc8c10fa99721351d2255de62938953b9ae7c9438fc1e1bc066c00003790cb6"),
            ("70", "73616c7473616c7473616c7473616c74", 8, 1, "55"),
            ("70617373776f7264", "73616c74", 3, 100, "cc1d220db52f6b0008dc05290209715c75ff32803593a632a9dd1fbf62875808f5d3829791ed0755d43a575aa72509a1eeb6a30aa0d538bc8f112883e1c6aa5b5ea190a0e01269e9b691bc41805006f0449475b5b1598585a7c8d105b2c50dd409c4e9a1"),
        ]
        for (p, s, rounds, n, want) in cases {
            let got = try BcryptPBKDF.derive(passphrase: hex(p), salt: hex(s), rounds: rounds, length: n)
            XCTAssertEqual(hexString(got), want, "rounds \(rounds) length \(n)")
        }
        XCTAssertThrowsError(try BcryptPBKDF.derive(passphrase: [1], salt: [1], rounds: 0, length: 32))
        XCTAssertThrowsError(try BcryptPBKDF.derive(passphrase: [1], salt: [], rounds: 1, length: 32))
    }

    // MARK: - Reading keys

    private struct Case {
        let name: String
        let text: String
        let pub: String
        let fp: String
        let type: ImportedKeyType
        let encrypted: Bool
        let comment: String
    }

    private var cases: [Case] {
        typealias F = KeyFixtures
        return [
            Case(name: "ed25519", text: F.ed25519, pub: F.ed25519_pub, fp: F.ed25519_fp, type: .ed25519, encrypted: false, comment: "test@example"),
            Case(name: "ed25519 aes256-ctr", text: F.ed25519_enc, pub: F.ed25519_enc_pub, fp: F.ed25519_enc_fp, type: .ed25519, encrypted: true, comment: "test@example"),
            Case(name: "ed25519 one round", text: F.ed25519_rounds1, pub: F.ed25519_rounds1_pub, fp: F.ed25519_rounds1_fp, type: .ed25519, encrypted: true, comment: "rounds1@example"),
            Case(name: "ed25519 aes256-gcm", text: F.ed25519_gcm, pub: F.ed25519_gcm_pub, fp: F.ed25519_gcm_fp, type: .ed25519, encrypted: true, comment: "test@example"),
            Case(name: "ed25519 aes128-cbc", text: F.ed25519_cbc, pub: F.ed25519_cbc_pub, fp: F.ed25519_cbc_fp, type: .ed25519, encrypted: true, comment: "test@example"),
            Case(name: "p256", text: F.ecdsa256, pub: F.ecdsa256_pub, fp: F.ecdsa256_fp, type: .p256, encrypted: false, comment: "ecdsa256@example"),
            Case(name: "p256 aes256-ctr", text: F.ecdsa256_enc, pub: F.ecdsa256_enc_pub, fp: F.ecdsa256_enc_fp, type: .p256, encrypted: true, comment: "ecdsa256@example"),
            Case(name: "p384", text: F.ecdsa384, pub: F.ecdsa384_pub, fp: F.ecdsa384_fp, type: .p384, encrypted: false, comment: "ecdsa384@example"),
            Case(name: "p384 aes256-ctr", text: F.ecdsa384_enc, pub: F.ecdsa384_enc_pub, fp: F.ecdsa384_enc_fp, type: .p384, encrypted: true, comment: "ecdsa384@example"),
            Case(name: "p521", text: F.ecdsa521, pub: F.ecdsa521_pub, fp: F.ecdsa521_fp, type: .p521, encrypted: false, comment: "ecdsa521@example"),
            Case(name: "p521 aes256-ctr", text: F.ecdsa521_enc, pub: F.ecdsa521_enc_pub, fp: F.ecdsa521_enc_fp, type: .p521, encrypted: true, comment: "ecdsa521@example"),
            Case(name: "p256 SEC1 PEM", text: F.ecdsa256_pem, pub: F.ecdsa256_pem_pub, fp: F.ecdsa256_pem_fp, type: .p256, encrypted: false, comment: ""),
            Case(name: "p384 PKCS#8", text: F.ecdsa384_pkcs8, pub: F.ecdsa384_pkcs8_pub, fp: F.ecdsa384_pkcs8_fp, type: .p384, encrypted: false, comment: ""),
        ]
    }

    func testEveryTypeImports() throws {
        for c in cases {
            let file = try OpenSSHKeyFile.parse(c.text)
            XCTAssertEqual(file.type, c.type, c.name)
            XCTAssertEqual(file.isEncrypted, c.encrypted, c.name)
            if c.encrypted {
                // The public key is readable before the passphrase.
                XCTAssertEqual(file.publicKey.map { String(openSSHPublicKey: $0) }, c.pub, c.name)
                XCTAssertThrowsError(try file.decrypt(passphrase: nil), c.name) { XCTAssertEqual($0 as? KeyImportError, .passphraseNeeded) }
            }
            let m = try file.decrypt(passphrase: c.encrypted ? KeyFixtures.passphrase : nil)
            XCTAssertEqual(m.type, c.type, c.name)
            XCTAssertEqual(String(openSSHPublicKey: m.publicKey), c.pub, c.name)
            XCTAssertEqual(SSHFingerprint.of(m.publicKey), c.fp, c.name)
            XCTAssertEqual(m.comment, c.comment, c.name)
            XCTAssertEqual(m.raw.count, c.type.scalarLength, c.name)
            // The key signs, and its public key verifies the signature.
            let data = Data("flightdeck".utf8)
            switch c.type {
            case .ed25519:
                let k = try Curve25519.Signing.PrivateKey(rawRepresentation: m.raw)
                XCTAssertTrue(k.publicKey.isValidSignature(try k.signature(for: data), for: data))
            case .p256:
                let k = try P256.Signing.PrivateKey(rawRepresentation: m.raw)
                XCTAssertTrue(k.publicKey.isValidSignature(try k.signature(for: data), for: data))
            case .p384, .p521:
                XCTAssertNoThrow(try m.privateKey(), c.name)
            }
        }
    }

    func testWrongPassphrase() throws {
        for text in [KeyFixtures.ed25519_enc, KeyFixtures.ed25519_gcm, KeyFixtures.ed25519_cbc, KeyFixtures.ecdsa256_enc] {
            let file = try OpenSSHKeyFile.parse(text)
            XCTAssertThrowsError(try file.decrypt(passphrase: "not the passphrase")) { e in
                XCTAssertEqual(e as? KeyImportError, .wrongPassphrase)
                XCTAssertEqual("\(e)", "Wrong passphrase. Try again.")
            }
        }
    }

    func testRSARefusedBeforeThePassphrase() {
        for text in [KeyFixtures.rsa, KeyFixtures.rsa_pem] {
            XCTAssertThrowsError(try OpenSSHKeyFile.parse(text)) { e in
                XCTAssertEqual(e as? KeyImportError, .rsa)
                XCTAssertTrue("\(e)".contains("ssh-keygen -t ed25519"))
            }
        }
    }

    func testUnsupportedCipherNamesItself() {
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(KeyFixtures.ed25519_chacha)) { e in
            XCTAssertEqual(e as? KeyImportError, .unsupportedCipher("chacha20-poly1305@openssh.com"))
            XCTAssertTrue("\(e)".contains("ssh-keygen -p"))
        }
    }

    func testEncryptedPEMRefused() {
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(KeyFixtures.ecdsa256_pem_enc)) { e in
            XCTAssertEqual(e as? KeyImportError, .encryptedPEM)
        }
        let label = "ENCRYPTED " + "PRIVATE KEY"
        XCTAssertThrowsError(try OpenSSHKeyFile.parse("-----BEGIN \(label)-----\nAAAA\n-----END \(label)-----\n")) { e in
            XCTAssertEqual(e as? KeyImportError, .encryptedPEM)
        }
    }

    func testEd25519PKCS8Imports() throws {
        let k = Curve25519.Signing.PrivateKey()
        let der = Data(PEMKey.ed25519PKCS8Prefix) + k.rawRepresentation
        let pem = "-----BEGIN PRIVATE KEY-----\n" + der.base64EncodedString() + "\n-----END PRIVATE KEY-----\n"
        let m = try OpenSSHKeyFile.parse(pem).decrypt(passphrase: nil)
        XCTAssertEqual(m.type, .ed25519)
        XCTAssertEqual(m.publicKey, NIOSSHPrivateKey(ed25519Key: k).publicKey)
    }

    func testPublicKeyAndOtherText() {
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(KeyFixtures.ed25519_pub + " test@example\n")) { e in
            XCTAssertEqual(e as? KeyImportError, .publicKeyOnly)
        }
        XCTAssertThrowsError(try OpenSSHKeyFile.parse("hello")) { e in
            XCTAssertEqual(e as? KeyImportError, .notAKey)
        }
    }

    func testTextAroundTheKeyIsIgnored() throws {
        let text = "Here is my key:\r\n\r\n" + KeyFixtures.ed25519.replacingOccurrences(of: "\n", with: "\r\n") + "\r\nthanks\n" + KeyFixtures.ecdsa256
        let m = try OpenSSHKeyFile.parse(text).decrypt(passphrase: nil)
        XCTAssertEqual(String(openSSHPublicKey: m.publicKey), KeyFixtures.ed25519_pub)
    }

    // MARK: - Damaged files

    /// The decoded container and where its private section starts and ends.
    private func container(_ text: String) throws -> (bytes: [UInt8], section: Range<Int>) {
        let inner = text
            .replacingOccurrences(of: OpenSSHKeyFile.begin, with: "")
            .replacingOccurrences(of: OpenSSHKeyFile.end, with: "")
            .filter { !$0.isWhitespace }
        let bytes = Array(try XCTUnwrap(Data(base64Encoded: inner)))
        var r = SSHWireReader(bytes, at: OpenSSHKeyFile.magic.count)
        _ = try r.string(); _ = try r.string(); _ = try r.string() // cipher, kdf, options
        _ = try r.u32()
        _ = try r.string() // public key
        let n = Int(try r.u32())
        return (bytes, r.offset..<r.offset + n)
    }

    private func armor(_ bytes: [UInt8]) -> String {
        OpenSSHKeyFile.begin + "\n" + Data(bytes).base64EncodedString(options: .lineLength64Characters) + "\n" + OpenSSHKeyFile.end + "\n"
    }

    func testCorruptedPadding() throws {
        // Unencrypted: the last byte is padding.
        var (b, sec) = try container(KeyFixtures.ed25519)
        b[sec.upperBound - 1] ^= 0x40
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(armor(b)).decrypt(passphrase: nil)) { e in
            XCTAssertEqual(e as? KeyImportError, .badPadding)
        }
        // aes256-ctr: flipping a ciphertext bit flips the same plaintext bit.
        (b, sec) = try container(KeyFixtures.ed25519_enc)
        b[sec.upperBound - 1] ^= 0x40
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(armor(b)).decrypt(passphrase: KeyFixtures.passphrase)) { e in
            XCTAssertEqual(e as? KeyImportError, .badPadding)
        }
    }

    func testCheckWordMismatch() throws {
        var (b, sec) = try container(KeyFixtures.ed25519)
        b[sec.lowerBound] ^= 0x01
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(armor(b)).decrypt(passphrase: nil)) { e in
            XCTAssertEqual(e as? KeyImportError, .checkMismatch)
        }
    }

    func testMismatchedPublicKey() throws {
        // One byte of the public key changed: the private key no longer matches it.
        var (b, sec) = try container(KeyFixtures.ed25519)
        b[sec.lowerBound - 8] ^= 0x01 // inside the public key blob
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(armor(b)).decrypt(passphrase: nil)) { e in
            guard case .malformed = e as? KeyImportError else { return XCTFail("\(e)") }
        }
    }

    func testTruncated() throws {
        let (b, sec) = try container(KeyFixtures.ed25519_enc)
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(armor(Array(b[0..<(sec.lowerBound + 20)])))) { e in
            XCTAssertEqual(e as? KeyImportError, .truncated)
        }
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(armor(Array(b[0..<30])))) { e in
            XCTAssertEqual(e as? KeyImportError, .truncated)
        }
        // No END line: the copy stopped short.
        let cut = String(KeyFixtures.ed25519.prefix(200))
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(cut)) { e in
            XCTAssertEqual(e as? KeyImportError, .truncated)
        }
    }

    // MARK: - The store

    func testImportedKeyStore() throws {
        let store = MemorySecretStore()
        let keys = DeviceKeys(store: store, preferEnclave: false)
        _ = try keys.defaultKey()
        let m = try OpenSSHKeyFile.parse(KeyFixtures.ed25519_enc).decrypt(passphrase: KeyFixtures.passphrase)
        let (k, replaced) = try keys.importKey(m)
        XCTAssertFalse(replaced)
        XCTAssertEqual(k.kind, .imported)
        XCTAssertEqual(k.fingerprint, KeyFixtures.ed25519_enc_fp)
        XCTAssertEqual(k.authorizedKeysLine, KeyFixtures.ed25519_enc_pub + " test@example")
        // The stored item holds the raw key, never the passphrase or the file.
        let blob = try XCTUnwrap(store.read(account: k.id))
        let text = String(decoding: blob, as: UTF8.self)
        XCTAssertFalse(text.contains(KeyFixtures.passphrase))
        XCTAssertFalse(text.contains("OPENSSH"))

        // A fresh DeviceKeys reads it back from the store.
        let again = DeviceKeys(store: store, preferEnclave: false)
        let loaded = try XCTUnwrap(try again.key(k.id) as? ImportedKey)
        XCTAssertEqual(loaded.publicKey, k.publicKey)
        XCTAssertEqual(loaded.comment, "test@example")
        XCTAssertEqual(try again.importedKeys().map(\.id), [k.id])
        XCTAssertNotNil(try again.key(DeviceKeys.defaultID) as? GeneratedKey)

        // The same key twice keeps one item.
        let (_, replaced2) = try again.importKey(m)
        XCTAssertTrue(replaced2)
        XCTAssertEqual(try again.importedKeys().count, 1)

        try again.setFaceID(true)
        XCTAssertTrue(try XCTUnwrap(try again.key(k.id)).requiresBiometry)
        try again.setFaceID(false)

        try again.delete(k.id)
        XCTAssertNil(try again.key(k.id))
        XCTAssertEqual(try again.importedKeys().count, 0)
        let e = SSHHabEntry(user: "u", endpoints: [SSHEndpoint(host: "h")], hostKeyFingerprint: KeyFixtures.ed25519_fp, keyID: k.id)
        XCTAssertThrowsError(try e.transport(keys: again)) { err in
            XCTAssertTrue("\(err)".contains("no key"))
        }
    }

    /// The Keychain storage class: usable only while unlocked, this device
    /// only, not synchronisable (app.md 10.4).
    func testKeychainStorageClass() throws {
        let store = KeychainStore(service: "flightdeck.test." + UUID().uuidString)
        let q = store.addQuery(account: "a", data: Data([1]))
        XCTAssertEqual(q[kSecAttrAccessible as String] as! CFString, kSecAttrAccessibleWhenUnlockedThisDeviceOnly)
        XCTAssertEqual(q[kSecAttrSynchronizable as String] as? Bool, false)

        // The real Keychain, when the test process may use it.
        do {
            try store.write(account: "imported-test", data: Data("x".utf8))
        } catch {
            throw XCTSkip("no keychain access here: \(error)")
        }
        defer { try? store.delete(account: "imported-test") }
        let p = try XCTUnwrap(try store.protectionOf(account: "imported-test"))
        XCTAssertFalse(p.synchronizable)
        #if os(iOS)
        // The Mac's login keychain keeps no accessibility class (app.md 10.6).
        XCTAssertEqual(p.accessible, kSecAttrAccessibleWhenUnlockedThisDeviceOnly as String)
        #endif
        XCTAssertEqual(try store.accounts(), ["imported-test"])
        try store.write(account: "imported-test", data: Data("y".utf8))
        XCTAssertEqual(try store.read(account: "imported-test"), Data("y".utf8))
        XCTAssertFalse(try XCTUnwrap(try store.protectionOf(account: "imported-test")).synchronizable)
    }

    // MARK: - On the carrier

    func testImportedKeyAuthenticatesOverSSH() async throws {
        let server = TestSSHServer(hab: await sampleHab())
        try await server.start()
        defer { server.stop() }
        let keys = DeviceKeys(store: MemorySecretStore(), preferEnclave: false)
        for text in [KeyFixtures.ed25519_enc, KeyFixtures.ecdsa384_enc] {
            let m = try OpenSSHKeyFile.parse(text).decrypt(passphrase: KeyFixtures.passphrase)
            let k = try keys.importKey(m).key
            server.allow(k.publicKey)
            let entry = SSHHabEntry(user: "tester", endpoints: [server.endpoint], hostKeyFingerprint: server.hostKeyFingerprint, keyID: k.id)
            let r = try await HabOneShot.call(try entry.transport(keys: keys, timing: fastSSH()), HabMethods.habPing, PingParams(), clientLabel: "test")
            XCTAssertGreaterThan(r.elapsed, .zero)
        }
        // The generated key is not allowed on this server: the carrier really used the imported one.
        let g = try keys.defaultKey()
        let entry = SSHHabEntry(user: "tester", endpoints: [server.endpoint], hostKeyFingerprint: server.hostKeyFingerprint, keyID: g.id)
        do {
            _ = try await entry.transport(keys: keys, timing: fastSSH()).open(.control)
            XCTFail("the generated key was accepted")
        } catch is SSHKeyRefused {}
    }

    // MARK: - Guards (fix round)

    /// The encrypted Ed25519 fixture with its bcrypt round count replaced.
    private func withRounds(_ rounds: UInt32) throws -> String {
        var (b, _) = try container(KeyFixtures.ed25519_enc)
        var r = SSHWireReader(b, at: OpenSSHKeyFile.magic.count)
        _ = try r.string(); _ = try r.string() // cipher, kdf
        let options = try r.string()
        let end = r.offset // the options end with the round count
        XCTAssertEqual(options.count, 4 + 16 + 4)
        b[end - 4] = UInt8(rounds >> 24); b[end - 3] = UInt8((rounds >> 16) & 0xFF)
        b[end - 2] = UInt8((rounds >> 8) & 0xFF); b[end - 1] = UInt8(rounds & 0xFF)
        return armor(b)
    }

    /// A crafted round count is refused when the file is read, before the
    /// passphrase and any work (review M1).
    func testTooManyRoundsRefusedAtOnce() throws {
        let start = Date()
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(withRounds(1 << 20))) { e in
            XCTAssertEqual(e as? KeyImportError, .tooManyRounds(1 << 20))
            XCTAssertTrue("\(e)".contains("ssh-keygen -p -a 100"))
        }
        XCTAssertLessThan(Date().timeIntervalSince(start), 1)
        XCTAssertTrue(try OpenSSHKeyFile.parse(withRounds(OpenSSHKeyFile.maxRounds)).isEncrypted)
    }

    /// A cancelled derivation stops between rounds (review M1).
    func testDerivationStopsWhenCancelled() async throws {
        let start = Date()
        let t = Task.detached { try BcryptPBKDF.derive(passphrase: [1], salt: [2], rounds: 1024, length: 48) }
        try await Task.sleep(for: .milliseconds(50))
        t.cancel()
        do {
            _ = try await t.value
            XCTFail("the derivation ran to the end")
        } catch is CancellationError {}
        XCTAssertLessThan(Date().timeIntervalSince(start), 3)
    }

    /// Cancelling the import (the sheet went away) stops the detached
    /// decryption too (review M1).
    @MainActor
    func testImportCancelStopsTheDerivation() async throws {
        let setup = SSHSetup(keys: DeviceKeys(store: MemorySecretStore(), preferEnclave: false), listURL: nil, fleet: nil)
        let file = try OpenSSHKeyFile.parse(withRounds(OpenSSHKeyFile.maxRounds))
        let start = Date()
        let t = Task { try await setup.importKey(file, passphrase: "anything") }
        try await Task.sleep(for: .milliseconds(100))
        t.cancel()
        do {
            _ = try await t.value
            XCTFail("the import finished")
        } catch is CancellationError {}
        XCTAssertLessThan(Date().timeIntervalSince(start), 3)
        XCTAssertTrue(setup.importedKeys.isEmpty)
    }

    /// Input over 64 KiB is refused without reading it all (review M2).
    func testOversizeInputRefused() throws {
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(Data(count: OpenSSHKeyFile.maxSize + 1))) { XCTAssertEqual($0 as? KeyImportError, .tooLarge) }
        XCTAssertThrowsError(try OpenSSHKeyFile.parse(String(repeating: "a", count: OpenSSHKeyFile.maxSize + 1))) { XCTAssertEqual($0 as? KeyImportError, .tooLarge) }
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("keyimport-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let big = dir.appendingPathComponent("video.mov")
        try Data(count: 4 << 20).write(to: big)
        XCTAssertThrowsError(try OpenSSHKeyFile.read(contentsOf: big)) { XCTAssertEqual($0 as? KeyImportError, .tooLarge) }
        let small = dir.appendingPathComponent("id_test")
        try Data(KeyFixtures.ed25519.utf8).write(to: small)
        XCTAssertEqual(try OpenSSHKeyFile.parse(OpenSSHKeyFile.read(contentsOf: small)).type, .ed25519)
    }

    /// A file that cannot be read says why, not "not a key" (review S1), and
    /// a large one is refused (review M2).
    @MainActor
    func testOpenKeyFileErrors() throws {
        let setup = SSHSetup(keys: DeviceKeys(store: MemorySecretStore(), preferEnclave: false), listURL: nil, fleet: nil)
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("keyimport-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        setup.openKeyFile(dir.appendingPathComponent("gone"))
        let e = try XCTUnwrap(setup.pendingKeyFile?.error)
        XCTAssertTrue(e.hasPrefix("Could not read the file"), e)
        let big = dir.appendingPathComponent("video.mov")
        try Data(count: 1 << 20).write(to: big)
        setup.openKeyFile(big)
        XCTAssertEqual(setup.pendingKeyFile?.error, KeyImportError.tooLarge.description)
        let ok = dir.appendingPathComponent("id_test")
        try Data(KeyFixtures.ed25519.utf8).write(to: ok)
        setup.openKeyFile(ok)
        XCTAssertNil(setup.pendingKeyFile?.error)
        XCTAssertFalse(setup.pendingKeyFile?.data.isEmpty ?? true)
    }

    /// One item that does not decode leaves the others listed (review S2).
    func testBadItemDoesNotHideOtherKeys() throws {
        let store = MemorySecretStore()
        let keys = DeviceKeys(store: store, preferEnclave: false)
        let good = try keys.importKey(try OpenSSHKeyFile.parse(KeyFixtures.ed25519).decrypt(passphrase: nil)).key
        try store.write(account: DeviceKeys.importedPrefix + "broken", data: Data("not json".utf8))
        let fresh = DeviceKeys(store: store, preferEnclave: false)
        XCTAssertEqual(try fresh.importedKeys().map(\.id), [good.id])
    }

    /// A locked device reads as locked, not as a missing key (review S3).
    @MainActor
    func testLockedKeyIsNotMissing() async throws {
        let store = LockedStore()
        let keys = DeviceKeys(store: store, preferEnclave: false)
        let setup = SSHSetup(keys: keys, listURL: nil, fleet: nil)
        let locked = SSHHabEntry(user: "u", endpoints: [SSHEndpoint(host: "h")], hostKeyFingerprint: KeyFixtures.ed25519_fp, keyID: DeviceKeys.importedPrefix + "present")
        guard case .unavailable(let why) = setup.keyState(locked) else { return XCTFail("\(setup.keyState(locked))") }
        XCTAssertTrue(why.contains("locked"), why)
        let gone = SSHHabEntry(user: "u", endpoints: [SSHEndpoint(host: "h")], hostKeyFingerprint: KeyFixtures.ed25519_fp, keyID: DeviceKeys.importedPrefix + "gone")
        XCTAssertEqual(setup.keyState(gone), .missing)
        do {
            _ = try await setup.probe(SSHDestination(user: "u", endpoint: SSHEndpoint(host: "127.0.0.1", port: 1)), keyID: locked.keyID)
            XCTFail("the probe ran with a locked key")
        } catch {
            XCTAssertTrue("\(error)".contains("locked"), "\(error)")
        }
        // The real Keychain's error keeps its status, so explain names the lock.
        XCTAssertTrue(DeviceKeys.explain(KeychainStore.error("read", errSecInteractionNotAllowed)).description.contains("locked"))
    }

    /// Turning Face ID off for an imported key asks for authentication;
    /// a refusal leaves it on (fix round 2).
    /// One setting covers the generated key and every imported key.
    @MainActor
    func testFaceIDOffNeedsAuthentication() async throws {
        let keys = DeviceKeys(store: MemorySecretStore(), preferEnclave: false)
        let setup = SSHSetup(keys: keys, listURL: nil, fleet: nil)
        setup.loadKey()
        let k = try keys.importKey(try OpenSSHKeyFile.parse(KeyFixtures.ed25519).decrypt(passphrase: nil)).key
        setup.loadImportedKeys()
        XCTAssertFalse(setup.faceID, "off by default")
        XCTAssertFalse(k.requiresBiometry)
        let asked = Counter()
        setup.authenticate = { _ in
            asked.add()
            throw CancellationError()
        }
        // A key loaded before the change follows it at its next use.
        let held = try XCTUnwrap(try keys.key(k.id))
        // On needs nothing, and covers both keys.
        try await setup.setFaceID(true)
        XCTAssertTrue(held.requiresBiometry, "read at use, not at load")
        XCTAssertEqual(asked.n, 0)
        XCTAssertTrue(setup.faceID)
        XCTAssertTrue(try XCTUnwrap(setup.importedKeys.first).requiresBiometry)
        XCTAssertTrue(try XCTUnwrap(setup.deviceKey).requiresBiometry)
        XCTAssertTrue(try XCTUnwrap(try keys.key(DeviceKeys.defaultID)).requiresBiometry)
        // A key imported while it is on needs it too.
        let k2 = try keys.importKey(try OpenSSHKeyFile.parse(KeyFixtures.ecdsa256).decrypt(passphrase: nil)).key
        XCTAssertTrue(k2.requiresBiometry)
        // Off with a refused authentication stays on.
        do {
            try await setup.setFaceID(false)
            XCTFail("Face ID was turned off without authentication")
        } catch {}
        XCTAssertEqual(asked.n, 1)
        XCTAssertTrue(setup.faceID)
        XCTAssertTrue(try keys.faceID())
        XCTAssertTrue(try XCTUnwrap(try keys.key(k.id)).requiresBiometry)
        // Off with authentication works for every key.
        setup.authenticate = { _ in asked.add() }
        try await setup.setFaceID(false)
        XCTAssertEqual(asked.n, 2)
        XCTAssertFalse(setup.faceID)
        XCTAssertFalse(try XCTUnwrap(setup.importedKeys.first).requiresBiometry)
        XCTAssertFalse(try XCTUnwrap(setup.deviceKey).requiresBiometry)
        // Off again asks nothing.
        try await setup.setFaceID(false)
        XCTAssertEqual(asked.n, 2)
    }

    /// The per-key flags of earlier builds become the one setting, and no
    /// stored key keeps its flag.
    func testPerKeyFaceIDMigrates() throws {
        let m = try OpenSSHKeyFile.parse(KeyFixtures.ed25519).decrypt(passphrase: nil)
        let raw = P256.Signing.PrivateKey().rawRepresentation
        func legacy(imported: Bool, device: Bool) throws -> MemorySecretStore {
            let store = MemorySecretStore()
            let id = DeviceKeys.importedID(for: m.publicKey)
            try store.write(account: id, data: JSONEncoder().encode(StoredImportedKey(type: m.type, raw: m.raw, comment: m.comment, biometry: imported, created: Date())))
            try store.write(account: DeviceKeys.defaultID, data: JSONEncoder().encode(StoredKey(kind: .software, data: raw, biometry: device, created: Date())))
            return store
        }
        func flags(_ store: MemorySecretStore) throws -> [String] {
            try store.accounts().filter { a in
                guard let d = try store.read(account: a), let o = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { return false }
                return o["biometry"] != nil
            }
        }
        for (imported, device, want) in [(true, false, true), (false, true, true), (false, false, false)] {
            let store = try legacy(imported: imported, device: device)
            XCTAssertEqual(try flags(store).count, 2)
            let keys = DeviceKeys(store: store, preferEnclave: false)
            XCTAssertEqual(try keys.faceID(), want, "imported \(imported), device \(device)")
            XCTAssertEqual(try flags(store), [], "no per-key flag left")
            XCTAssertEqual(try keys.defaultKey().requiresBiometry, want)
            XCTAssertEqual(try keys.importedKeys().first?.requiresBiometry, want)
            // A second start reads the same and writes nothing new.
            XCTAssertEqual(try DeviceKeys(store: store, preferEnclave: false).faceID(), want)
            XCTAssertEqual(try store.accounts().count, want ? 3 : 2)
        }
    }

    /// An enclave key made with Face ID built in (an earlier build's
    /// switch) is told apart by one signature with no interaction, and
    /// still needs Face ID with the setting off; a plain one does not.
    func testLegacyEnclaveKeyIsDetected() throws {
        guard DeviceKeys.enclaveAvailable else { throw XCTSkip("no Secure Enclave") }
        func enclaveKey(_ flags: SecAccessControlCreateFlags) throws -> Data {
            let access = try XCTUnwrap(SecAccessControlCreateWithFlags(nil, kSecAttrAccessibleWhenUnlockedThisDeviceOnly, flags, nil))
            do {
                return try SecureEnclave.P256.Signing.PrivateKey(accessControl: access).dataRepresentation
            } catch {
                throw XCTSkip("no enclave key here: \(error)")
            }
        }
        let plain = try enclaveKey([.privateKeyUsage])
        let built: Data
        do {
            built = try enclaveKey([.privateKeyUsage, .biometryCurrentSet])
        } catch {
            throw XCTSkip("no biometric enclave key here (no Touch ID enrolled?): \(error)")
        }
        XCTAssertFalse(GeneratedKey.enclaveNeedsUser(plain))
        XCTAssertTrue(GeneratedKey.enclaveNeedsUser(built))

        let store = MemorySecretStore()
        try store.write(account: DeviceKeys.defaultID, data: JSONEncoder().encode(StoredKey(kind: .secureEnclave, data: built, biometry: true, created: Date())))
        let keys = DeviceKeys(store: store)
        XCTAssertTrue(try keys.faceID(), "the old switch was on")
        try keys.setFaceID(false)
        let k = try keys.defaultKey()
        XCTAssertTrue(k.biometryBuiltIn)
        XCTAssertTrue(k.requiresBiometry, "the enclave still asks")
        let p = try DeviceKeys(store: { let s = MemorySecretStore(); try? s.write(account: DeviceKeys.defaultID, data: JSONEncoder().encode(StoredKey(kind: .secureEnclave, data: plain, created: Date()))); return s }()).defaultKey()
        XCTAssertFalse(p.requiresBiometry)
    }

    /// Only "interaction not allowed" means Face ID; other failures do not.
    func testOnlyNotInteractiveMeansFaceID() {
        let la = NSError(domain: LAError.errorDomain, code: LAError.Code.notInteractive.rawValue)
        XCTAssertTrue(GeneratedKey.isNotInteractive(la))
        XCTAssertTrue(GeneratedKey.isNotInteractive(NSError(domain: "CryptoTokenKit", code: -3, userInfo: [NSUnderlyingErrorKey: la])))
        XCTAssertFalse(GeneratedKey.isNotInteractive(NSError(domain: NSOSStatusErrorDomain, code: Int(errSecInteractionNotAllowed))))
        XCTAssertFalse(GeneratedKey.isNotInteractive(NSError(domain: LAError.errorDomain, code: LAError.Code.biometryNotEnrolled.rawValue)))
    }

    /// The Inbox copy is found when the file URL and the home folder spell
    /// /var differently (fix round 2).
    func testInboxCopyMatchesAcrossPrivateVar() throws {
        let home = FileManager.default.temporaryDirectory.appendingPathComponent("home-" + UUID().uuidString)
        let inbox = home.appendingPathComponent("tmp/com.example.app-Inbox")
        try FileManager.default.createDirectory(at: inbox, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: home) }
        let file = inbox.appendingPathComponent("id_test")
        try Data("x".utf8).write(to: file)
        let resolved = file.resolvingSymlinksInPath().path
        let homeVar = home.path.hasPrefix("/private/") ? String(home.path.dropFirst("/private".count)) : home.path
        let filePrivate = resolved.hasPrefix("/private/") ? resolved : "/private" + resolved
        guard FileManager.default.fileExists(atPath: filePrivate), homeVar.hasPrefix("/var/") else {
            throw XCTSkip("the temporary folder is not under /var here")
        }
        XCTAssertTrue(SSHSetup.isOwnInboxCopy(URL(fileURLWithPath: filePrivate), home: homeVar))
        XCTAssertTrue(SSHSetup.isOwnInboxCopy(URL(fileURLWithPath: homeVar + "/Documents/Inbox/k"), home: "/private" + homeVar))
        XCTAssertFalse(SSHSetup.isOwnInboxCopy(URL(fileURLWithPath: homeVar + "/Documents/notes/k"), home: homeVar))
        XCTAssertFalse(SSHSetup.isOwnInboxCopy(URL(fileURLWithPath: "/var/other-Inbox/k"), home: homeVar))
        XCTAssertFalse(SSHSetup.isOwnInboxCopy(URL(fileURLWithPath: homeVar + "x/tmp/a-Inbox/k"), home: homeVar))
    }

    /// A pairing link and a key file arriving together are shown in turn
    /// (review N2).
    @MainActor
    func testPairingAndKeyFileShownInTurn() throws {
        let setup = SSHSetup(keys: DeviceKeys(store: MemorySecretStore(), preferEnclave: false), listURL: nil, fleet: nil)
        setup.pendingKeyFile = PendingKeyFile(name: "k", data: Data())
        setup.pendingCodeError = "bad code"
        guard case .pairing = SSHSheets.item(setup) else { return XCTFail("pairing first") }
        setup.pendingCodeError = nil
        guard case .keyFile = SSHSheets.item(setup) else { return XCTFail("then the key file") }
        setup.pendingKeyFile = nil
        XCTAssertNil(SSHSheets.item(setup))
    }
}

private final class Counter: @unchecked Sendable {
    private let lock = NSLock()
    private var value = 0
    var n: Int { lock.withLock { value } }
    func add() { lock.withLock { value += 1 } }
}

/// A store whose items exist but cannot be read: a locked device.
private final class LockedStore: SecretStore, @unchecked Sendable {
    func read(account: String) throws -> Data? {
        guard account.hasSuffix("present") else { return nil }
        throw NSError(domain: NSOSStatusErrorDomain, code: Int(errSecInteractionNotAllowed))
    }
    func write(account: String, data: Data) throws {}
    func delete(account: String) throws {}
    func accounts() throws -> [String] { [DeviceKeys.importedPrefix + "present"] }
}
