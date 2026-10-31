import Foundation
import XCTest
@testable import HabAPI

final class HabAPITests: XCTestCase {
    func testWelcomeDecodes() throws {
        let json = """
        {"seq":0,"revision":1,"epoch":"e1","build":"dev","host":{"id":"h1"},
         "capabilities":["data-streams","future-thing"],"client_id":"c1","reconnect_token":"t1",
         "added_later":true}
        """
        let w = try JSONDecoder().decode(Welcome.self, from: Data(json.utf8))
        XCTAssertEqual(w.clientId, "c1")
        XCTAssertEqual(w.capabilities.count, 2)
    }

    func testUnknownWordRoundTrips() throws {
        let json = #"{"kind":"brand_new","hint":"h","seq":3}"#
        let e = try JSONDecoder().decode(ErrorData.self, from: Data(json.utf8))
        XCTAssertEqual(e.kind.rawValue, "brand_new")
        XCTAssertNotEqual(e.kind, .notFound)
    }

    func testEventDecodesByType() throws {
        let json = #"{"seq":1,"type":"pane.owner","id":"p1","data":{"reason":"typed","cols":80,"rows":24}}"#
        let event = try JSONDecoder().decode(Event.self, from: Data(json.utf8))
        guard case .paneOwner(let owner) = try HabEvent(event) else {
            return XCTFail("wrong case")
        }
        XCTAssertEqual(owner.reason, .typed)
        XCTAssertNil(owner.owner)
    }

    func testUnknownEventType() throws {
        let json = #"{"seq":2,"type":"later.thing","id":"x","revision":1,"data":{}}"#
        let event = try JSONDecoder().decode(Event.self, from: Data(json.utf8))
        guard case .unknown(let type, _) = try HabEvent(event) else {
            return XCTFail("wrong case")
        }
        XCTAssertEqual(type, "later.thing")
    }

    func testLargeOffsetSurvivesEventDecoding() throws {
        // 2^53 + 1 cannot be held by a Double.
        let json = #"{"seq":1,"type":"pane.updated","id":"p1","revision":9007199254740993,"data":{"id":"p1","revision":9007199254740993,"holder_build":"b","cols":80,"rows":24,"offset":18446744073709551615}}"#
        let event = try JSONDecoder().decode(Event.self, from: Data(json.utf8))
        XCTAssertEqual(event.revision, 9_007_199_254_740_993)
        guard case .paneUpdated(let pane) = try HabEvent(event) else {
            return XCTFail("wrong case")
        }
        XCTAssertEqual(pane.offset, UInt64.max)
        XCTAssertEqual(pane.revision, 9_007_199_254_740_993)
    }

    func testOwnerEventHasNoRevision() throws {
        let json = #"{"seq":3,"type":"pane.owner","id":"p1","data":{"reason":"take","cols":80,"rows":24}}"#
        let event = try JSONDecoder().decode(Event.self, from: Data(json.utf8))
        XCTAssertNil(event.revision)
    }

    func testInputDataIsBase64() throws {
        let p = InputParams(data: Data("hi".utf8), pane: "p1")
        let s = String(decoding: try JSONEncoder().encode(p), as: UTF8.self)
        XCTAssertTrue(s.contains("\"aGk=\""), s)
    }
}
