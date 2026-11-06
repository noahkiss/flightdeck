import Foundation
import NIOSSH
import XCTest
@testable import FlightdeckKit

/// QR pairing against a real `gff-hab pair` window and a throwaway sshd,
/// run by `apple/scripts/pair-live-check.sh`. Skipped when its variables are
/// unset.
final class LivePairTests: XCTestCase {
    @MainActor
    func testLivePairing() async throws {
        let env = ProcessInfo.processInfo.environment
        guard let link = env["FLIGHTDECK_LIVE_PAIR_LINK"], let codeFile = env["FLIGHTDECK_LIVE_PAIR_CODE_FILE"],
              let hostKeyFile = env["FLIGHTDECK_LIVE_PAIR_SSHD_HOSTKEY"], let endpoint = env["FLIGHTDECK_LIVE_PAIR_SSHD_ENDPOINT"]
        else {
            throw XCTSkip("FLIGHTDECK_LIVE_PAIR_* not set: run apple/scripts/pair-live-check.sh")
        }
        let code = try PairingCode.parse(link)
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("fd-livepair-" + UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        let setup = SSHSetup(keys: DeviceKeys(store: MemorySecretStore(), preferEnclave: false), listURL: dir.appendingPathComponent("ssh-habs.json"), fleet: nil)
        XCTAssertNotNil(setup.loadKey())

        let start = Date()
        let shown = Box<String>()
        let r = try await setup.pair(code, name: "Live test phone", kind: .phone) { fp in
            shown.set(fp)
            // The owner reads these off the phone; the script types them at the window.
            try? Data(Pairing.confirmCode(fp).utf8).write(to: URL(fileURLWithPath: codeFile))
        }
        let paired = Date().timeIntervalSince(start)
        XCTAssertEqual(shown.get(), setup.deviceKey?.fingerprint)

        let sshdKey = try String(contentsOfFile: hostKeyFile, encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines)
        let pub = try NIOSSHPublicKey(openSSHPublicKey: sshdKey)
        XCTAssertFalse(r.needsHostKey)
        XCTAssertEqual(r.entry.hostKeyFingerprint, SSHFingerprint.of(pub))
        XCTAssertEqual(r.entry.endpoints, [SSHEndpoint.parse(endpoint)!])
        XCTAssertEqual(r.entry.deviceID, code.deviceID)
        XCTAssertEqual(r.entry.label, "live-box")
        XCTAssertNil(r.entry.command)

        let t0 = Date()
        try await setup.finishPairing(r.entry)
        print("live-pair: paired in \(Int(paired * 1000)) ms (owner included), first hab.ping over the forced command in \(Int(Date().timeIntervalSince(t0) * 1000)) ms")
        XCTAssertEqual(setup.entries.map(\.id), [r.entry.id])
        XCTAssertEqual(try SSHHabList.load(from: dir.appendingPathComponent("ssh-habs.json")).habs, [r.entry])
    }
}
