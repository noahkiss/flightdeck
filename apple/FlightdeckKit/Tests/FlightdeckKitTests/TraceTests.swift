import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The app sends a trace on each request (diagnostics.md 5.2): a new `a` trace per call.
final class TraceTests: XCTestCase {
    func testNewTraceForm() {
        var seen = Set<String>()
        for _ in 0..<200 {
            let t = Trace.new()
            XCTAssertTrue(Trace.isValid(t), t)
            XCTAssertEqual(t.first, "a")
            seen.insert(t)
        }
        XCTAssertEqual(seen.count, 200, "traces repeat")
        for bad in ["", "a0123456789", "x0123456789A", "a0123456789a", "a012345678IL"] {
            XCTAssertFalse(Trace.isValid(bad), bad)
        }
    }

    func testEachRequestCarriesATrace() async throws {
        let (client, server) = FakeStream.pair()
        let rpc = RPCClient(stream: client)
        await rpc.start()
        let hab = Task { () -> [String] in
            var traces: [String] = []
            var buf = Data()
            while traces.count < 2, let chunk = try await server.read() {
                buf.append(chunk)
                while let nl = buf.firstIndex(of: 0x0A) {
                    let line = buf[buf.startIndex..<nl]
                    buf = Data(buf[(nl + 1)...])
                    let obj = try JSONSerialization.jsonObject(with: line) as? [String: Any]
                    let id = obj?["id"] as? Int ?? 0
                    let trace = obj?["trace"] as? String ?? ""
                    traces.append(trace)
                    try server.send(Data("{\"jsonrpc\":\"2.0\",\"id\":\(id),\"trace\":\"\(trace)\",\"result\":{\"seq\":1}}\n".utf8))
                }
            }
            return traces
        }
        for _ in 0..<2 {
            _ = try await rpc.call(HabMethods.habPing, PingParams(), timeout: .seconds(5))
        }
        let traces = try await hab.value
        XCTAssertEqual(traces.count, 2)
        XCTAssertTrue(traces.allSatisfy { Trace.isValid($0) && $0.first == "a" }, "\(traces)")
        XCTAssertNotEqual(traces[0], traces[1], "a call reused the last trace")
        await rpc.close()
    }
}
