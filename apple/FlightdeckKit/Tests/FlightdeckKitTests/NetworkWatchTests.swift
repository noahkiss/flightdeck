import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Reconnect on a network change (app.md 10.2).
@MainActor
final class NetworkWatchTests: XCTestCase {
    private let wifi = NetworkPathReading(usable: true, interfaces: ["en0"])
    private let cell = NetworkPathReading(usable: true, interfaces: ["pdp_ip0"])
    private let none = NetworkPathReading(usable: false, interfaces: [])

    func testRule() {
        XCTAssertFalse(NetworkWatch.retries(after: nil, wifi), "the first reading is the state at start")
        XCTAssertTrue(NetworkWatch.retries(after: none, wifi), "the network came back")
        XCTAssertTrue(NetworkWatch.retries(after: wifi, cell), "Wi-Fi to cellular")
        XCTAssertFalse(NetworkWatch.retries(after: wifi, wifi), "nothing changed")
        XCTAssertFalse(NetworkWatch.retries(after: wifi, none), "no network: nothing to retry on")
        XCTAssertEqual(NetworkPathReading(usable: true, interfaces: ["b", "a"]), NetworkPathReading(usable: true, interfaces: ["a", "b"]))
    }

    func testFleetRetriesOnAChangeWhileVisible() async {
        let f = Fleet()
        let (stream, sink) = AsyncStream.makeStream(of: NetworkPathReading.self)
        f.watchNetwork(stream)
        await f.setActive(true)
        sink.yield(wifi)
        sink.yield(none)
        sink.yield(wifi)
        await eventually("one retry") { f.retries == 1 }
        sink.yield(cell)
        await eventually("two retries") { f.retries == 2 }

        // In the background a change retries nothing: the connections are closed (app.md 12.1).
        await f.setActive(false)
        sink.yield(wifi)
        sink.yield(none)
        sink.yield(wifi)
        try? await Task.sleep(for: .milliseconds(200))
        XCTAssertEqual(f.retries, 2)
        sink.finish()
    }

    func testRefreshRetriesAndWaitsForTheHabs() async {
        let hab = await sampleHab("bench")
        let f = Fleet()
        f.add(HabConnection(id: "bench", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await f.setActive(true)
        await f.refresh(limit: .seconds(5))
        XCTAssertEqual(f.retries, 1)
        XCTAssertTrue(f.board.habs.allSatisfy { $0.state.status == .online })
        await f.setActive(false)
    }
}
