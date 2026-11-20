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

    /// A failed call's error carries the hab's trace and prints it, and the app's log line names
    /// the method, the error kind and the trace, all public (5.2). A hab answer whose trace is
    /// not in a trace's form falls back to the trace the app sent.
    func testFailedCallCarriesTrace() async throws {
        let (client, server) = FakeStream.pair()
        let rpc = RPCClient(stream: client)
        await rpc.start()
        let hab = Task { () -> [String] in
            var sent: [String] = []
            var buf = Data()
            let answers = ["hABCDEFGHJK0", "not a trace"]
            while sent.count < 2, let chunk = try await server.read() {
                buf.append(chunk)
                while let nl = buf.firstIndex(of: 0x0A) {
                    let line = buf[buf.startIndex..<nl]
                    buf = Data(buf[(nl + 1)...])
                    let obj = try JSONSerialization.jsonObject(with: line) as? [String: Any]
                    let id = obj?["id"] as? Int ?? 0
                    sent.append(obj?["trace"] as? String ?? "")
                    let tr = answers[sent.count - 1]
                    try server.send(Data("{\"jsonrpc\":\"2.0\",\"id\":\(id),\"trace\":\"\(tr)\",\"error\":{\"code\":-32000,\"message\":\"no\",\"data\":{\"kind\":\"not_found\",\"hint\":\"no session\",\"seq\":1}}}\n".utf8))
                }
            }
            return sent
        }
        var errs: [RPCError] = []
        for _ in 0..<2 {
            do {
                _ = try await rpc.call(HabMethods.habPing, PingParams(), timeout: .seconds(5))
                XCTFail("the call succeeded")
            } catch let e as RPCError {
                errs.append(e)
            }
        }
        let sent = try await hab.value
        XCTAssertEqual(errs.count, 2)
        XCTAssertEqual(errs[0].trace, "hABCDEFGHJK0")
        XCTAssertTrue(errs[0].description.hasSuffix(" (trace hABCDEFGHJK0)"), errs[0].description)
        XCTAssertEqual(errs[1].trace, sent[1], "a malformed answer falls back to the trace sent")
        let line = RPCClient.failedCall("hab.ping", errs[0], trace: "hABCDEFGHJK0").redacted
        XCTAssertEqual(line, "call failed: hab.ping, RPCError not_found, trace hABCDEFGHJK0")
        await rpc.close()
    }
}
