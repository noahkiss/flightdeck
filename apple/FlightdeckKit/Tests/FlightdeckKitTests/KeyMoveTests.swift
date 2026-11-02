import CryptoKit
import FlightdeckShared
import Foundation
import HabAPI
import NIOSSH
import XCTest
@testable import FlightdeckKit

/// The device key that signs while the device is locked, and the move of an
/// earlier build's key (transport.md 7.2). The real lock state cannot be made
/// in a test: the store and the lock answers are stand-ins.
@MainActor
final class KeyMoveTests: XCTestCase {
    /// A default key as an earlier build left it: a key that signs only
    /// while unlocked (a software stand-in for its enclave key).
    func legacyStore() throws -> (MemorySecretStore, Data) {
        let store = MemorySecretStore()
        let raw = P256.Signing.PrivateKey().rawRepresentation
        let k = StoredKey(kind: .software, data: raw, created: Date(), access: StoredKey.whenUnlocked)
        try store.write(account: DeviceKeys.defaultID, data: JSONEncoder().encode(k))
        return (store, raw)
    }

    func tempListURL() -> URL {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("keymove-" + UUID().uuidString)
        addTeardownBlock { try? FileManager.default.removeItem(at: dir) }
        return dir.appendingPathComponent("ssh-habs.json")
    }

    func testNewKeysSignWhileLocked() throws {
        let store = MemorySecretStore()
        let keys = DeviceKeys(store: store, preferEnclave: false)
        let k = try keys.defaultKey()
        XCTAssertTrue(k.signsWhileLocked)
        let stored = try JSONDecoder().decode(StoredKey.self, from: XCTUnwrap(try store.read(account: DeviceKeys.defaultID)))
        XCTAssertEqual(stored.access, StoredKey.afterFirstUnlock)
        XCTAssertNil(try keys.startKeyMove(), "a key that signs while locked needs no move")
        XCTAssertEqual(keys.lockedSigning(keyID: DeviceKeys.defaultID, entry: "e1"), .ready)
        // An earlier build's software key has no access field and follows its item.
        let old = StoredKey(kind: .software, data: P256.Signing.PrivateKey().rawRepresentation, created: Date())
        XCTAssertTrue(old.signsWhileLocked)
        XCTAssertFalse(StoredKey(kind: .secureEnclave, data: Data(), created: Date()).signsWhileLocked)
    }

    func testMoveOffersBothKeysThenFinishes() async throws {
        let (store, oldRaw) = try legacyStore()
        let keys = DeviceKeys(store: store, preferEnclave: false)
        let old = try keys.defaultKey()
        XCTAssertFalse(old.signsWhileLocked)
        XCTAssertEqual(keys.lockedSigning(keyID: DeviceKeys.defaultID, entry: "e1"), .keyLocked)

        let next = try XCTUnwrap(try keys.startKeyMove())
        XCTAssertTrue(next.signsWhileLocked)
        XCTAssertEqual(try keys.startKeyMove()?.fingerprint, next.fingerprint, "a second start keeps the new key")
        let src = try XCTUnwrap(try keys.signingSource(keyID: DeviceKeys.defaultID, entry: "e1"))
        let offers = try await src.signingKeys()
        XCTAssertEqual(offers.map(\.fingerprint), [next.fingerprint, old.fingerprint])
        XCTAssertEqual(keys.lockedSigning(keyID: DeviceKeys.defaultID, entry: "e1"), .keyLocked)

        // The host took the old key: nothing recorded.
        src.accepted(fingerprint: old.fingerprint)
        XCTAssertEqual(try keys.movedEntries(), [])
        src.accepted(fingerprint: next.fingerprint)
        XCTAssertEqual(try keys.movedEntries(), ["e1"])
        XCTAssertEqual(keys.lockedSigning(keyID: DeviceKeys.defaultID, entry: "e1"), .ready)
        XCTAssertEqual(keys.lockedSigning(keyID: DeviceKeys.defaultID, entry: "e2"), .keyLocked)

        try keys.finishKeyMove()
        let d = try keys.defaultKey()
        XCTAssertEqual(d.fingerprint, next.fingerprint)
        XCTAssertTrue(d.signsWhileLocked)
        XCTAssertNil(try keys.nextKey())
        XCTAssertNil(try store.read(account: DeviceKeys.moveAccount))
        XCTAssertNotEqual(try JSONDecoder().decode(StoredKey.self, from: XCTUnwrap(try store.read(account: DeviceKeys.defaultID))).data, oldRaw)
        // A fresh start reads the same: no move.
        let again = DeviceKeys(store: store, preferEnclave: false)
        XCTAssertNil(try again.startKeyMove())
        XCTAssertEqual(try again.defaultKey().fingerprint, next.fingerprint)
    }

    /// The setting accounts are never read as keys, and the Face ID
    /// migration skips the move record.
    func testMoveRecordIsNotAKey() throws {
        let (store, _) = try legacyStore()
        let keys = DeviceKeys(store: store, preferEnclave: false)
        try keys.startKeyMove()
        keys.noteMoved(entry: "e1")
        XCTAssertNil(try keys.key(DeviceKeys.moveAccount))
        XCTAssertFalse(try DeviceKeys(store: store, preferEnclave: false).faceID())
        XCTAssertEqual(try keys.importedKeys().count, 0)
    }

    /// A store that cannot be read is the window before the first unlock.
    func testUnreadableStoreIsBeforeFirstUnlock() {
        let keys = DeviceKeys(store: BootLockedStore(), preferEnclave: false)
        XCTAssertFalse(keys.storeReadable())
        XCTAssertEqual(keys.lockedSigning(keyID: DeviceKeys.defaultID, entry: "e1"), .beforeFirstUnlock)
        XCTAssertEqual(keys.lockedSigning(keyID: "imported-x", entry: "e1"), .beforeFirstUnlock)
        XCTAssertTrue(DeviceKeys(store: MemorySecretStore(), preferEnclave: false).storeReadable())
    }

    /// SSHSetup starts the move, lists the hosts still waiting, and makes the
    /// new key the default key once every host on it signed in with it.
    func testSetupMovesWhenEveryHostTookTheNewKey() throws {
        let (store, _) = try legacyStore()
        let keys = DeviceKeys(store: store, preferEnclave: false)
        let url = tempListURL()
        let e1 = SSHHabEntry(label: "one", user: "u", endpoints: [SSHEndpoint(host: "a")], hostKeyFingerprint: "SHA256:x")
        let e2 = SSHHabEntry(label: "two", user: "u", endpoints: [SSHEndpoint(host: "b")], hostKeyFingerprint: "SHA256:y")
        let e3 = SSHHabEntry(label: "three", user: "u", endpoints: [SSHEndpoint(host: "c")], hostKeyFingerprint: "SHA256:z", keyID: "imported-other")
        try SSHHabList(habs: [e1, e2, e3]).save(to: url)
        let setup = SSHSetup(keys: keys, listURL: url, fleet: nil)
        setup.movesOldKey = true
        let old = try XCTUnwrap(setup.loadKey())
        let move = try XCTUnwrap(setup.keyMove)
        XCTAssertNotEqual(move.fingerprint, old.fingerprint)
        XCTAssertEqual(move.waiting.map(\.label), ["one", "two"], "only entries on the default key wait")
        XCTAssertEqual(setup.authorizedKeysLine, move.line, "a host added now gets the new key")

        keys.noteMoved(entry: e1.id)
        setup.refreshKeyMove()
        XCTAssertEqual(setup.keyMove?.waiting.map(\.label), ["two"])
        XCTAssertEqual(setup.deviceKey?.fingerprint, old.fingerprint)

        keys.noteMoved(entry: e2.id)
        setup.refreshKeyMove()
        XCTAssertNil(setup.keyMove)
        XCTAssertEqual(setup.deviceKey?.fingerprint, move.fingerprint)
        XCTAssertTrue(try XCTUnwrap(setup.deviceKey).signsWhileLocked)
    }

    /// Making a new key during a move ends the move: the new key signs while locked.
    func testReplaceKeyEndsMove() throws {
        let (store, _) = try legacyStore()
        let keys = DeviceKeys(store: store, preferEnclave: false)
        let url = tempListURL()
        try SSHHabList(habs: [SSHHabEntry(user: "u", endpoints: [SSHEndpoint(host: "a")], hostKeyFingerprint: "SHA256:x")]).save(to: url)
        let setup = SSHSetup(keys: keys, listURL: url, fleet: nil)
        setup.movesOldKey = true
        setup.loadKey()
        XCTAssertNotNil(setup.keyMove)
        try setup.replaceKey()
        XCTAssertNil(setup.keyMove)
        XCTAssertNil(try keys.nextKey())
        XCTAssertTrue(try XCTUnwrap(setup.deviceKey).signsWhileLocked)
    }

    /// Off (the Mac), an earlier build's key stays as it is.
    func testNoMoveWhenOff() throws {
        let (store, _) = try legacyStore()
        let setup = SSHSetup(keys: DeviceKeys(store: store, preferEnclave: false), listURL: tempListURL(), fleet: nil)
        setup.movesOldKey = false
        setup.loadKey()
        XCTAssertNil(setup.keyMove)
        XCTAssertNil(try store.read(account: DeviceKeys.nextID))
    }

    // MARK: - On the carrier

    /// A host that has only the old key still lets the device in; a host
    /// with the new key records the move.
    func testCarrierOffersNewKeyThenOld() async throws {
        let (store, _) = try legacyStore()
        let keys = DeviceKeys(store: store, preferEnclave: false)
        let old = try keys.defaultKey()
        let next = try XCTUnwrap(try keys.startKeyMove())

        let a = TestSSHServer(hab: await sampleHab())
        try await a.start()
        defer { a.stop() }
        a.allow(old.publicKey)
        let ea = SSHHabEntry(user: "tester", endpoints: [a.endpoint], hostKeyFingerprint: a.hostKeyFingerprint)
        _ = try await HabOneShot.call(try ea.transport(keys: keys, timing: fastSSH()), HabMethods.habPing, PingParams(), clientLabel: "test")
        XCTAssertEqual(try keys.movedEntries(), [], "the old key got in; nothing moved")

        let b = TestSSHServer(hab: await sampleHab())
        try await b.start()
        defer { b.stop() }
        b.allow(next.publicKey)
        let eb = SSHHabEntry(user: "tester", endpoints: [b.endpoint], hostKeyFingerprint: b.hostKeyFingerprint)
        _ = try await HabOneShot.call(try eb.transport(keys: keys, timing: fastSSH()), HabMethods.habPing, PingParams(), clientLabel: "test")
        XCTAssertEqual(try keys.movedEntries(), [eb.id])
    }
}

/// The Keychain before the first unlock after a restart: every call fails.
final class BootLockedStore: SecretStore, @unchecked Sendable {
    func read(account: String) throws -> Data? { throw KeychainStore.error("read", errSecInteractionNotAllowed) }
    func write(account: String, data: Data) throws { throw KeychainStore.error("write", errSecInteractionNotAllowed) }
    func delete(account: String) throws { throw KeychainStore.error("delete", errSecInteractionNotAllowed) }
    func accounts() throws -> [String] { throw KeychainStore.error("list", errSecInteractionNotAllowed) }
}

@MainActor
final class LockedAnswerTests: XCTestCase {
    /// Locked after the first unlock with a key that signs while locked: the
    /// Watch's answer goes through, and nothing is posted (app.md 16.4).
    func testLockedAnswerSigns() async {
        let hab = await blockedHab()
        let answerer = ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in hab.transport() }, clientLabel: "phone")
        let c = ApprovalCenter(board: Board(), answerer: answerer, defaults: emptyDefaults())
        var posts: [String] = []
        c.post = { t, _, _ in posts.append(t) }
        c.isUnlocked = { false }
        var asked: [String] = []
        c.lockedSigning = { host in asked.append(host); return .ready }
        let meta = PushMeta(kind: "permission", hab: "testhost", pane: "%1", request: "req-1")
        let o = await c.answer(host: "testhost", ApprovalAnswerParams(choice: .allow, pane: "%1", request: "req-1", via: .banner), meta: meta)
        XCTAssertEqual(o, .answered)
        XCTAssertEqual(asked, ["testhost"])
        XCTAssertEqual(posts, [])
        XCTAssertEqual(c.pending, [])
        let n = await hab.answers.count
        XCTAssertEqual(n, 1)
    }

    /// Unlocked, the lock answer is never asked.
    func testUnlockedNeverAsks() async {
        let hab = await blockedHab()
        let answerer = ApprovalAnswerer(live: { _ in nil }, oneShot: { _ in hab.transport() }, clientLabel: "phone")
        let c = ApprovalCenter(board: Board(), answerer: answerer, defaults: emptyDefaults())
        c.isUnlocked = { true }
        c.lockedSigning = { _ in XCTFail("asked while unlocked"); return .beforeFirstUnlock }
        let o = await c.answerFromActivity(host: "testhost", pane: "%1", request: "req-1", allow: false)
        XCTAssertEqual(o, .answered)
    }

    /// Each refusal names its case.
    func testRefusalTexts() {
        let before = ApprovalCenter.lockedNotice(.beforeFirstUnlock, hab: "web")
        XCTAssertEqual(before.title, ApprovalCenter.lockedTitle)
        XCTAssertTrue(before.body.contains("restarted"), before.body)
        XCTAssertTrue(before.body.contains("web"))
        let key = ApprovalCenter.lockedNotice(.keyLocked, hab: "web")
        XCTAssertEqual(key.body, "web does not have this device's new key yet")
    }
}
