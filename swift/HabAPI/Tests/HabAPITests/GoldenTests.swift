import Foundation
import XCTest
@testable import HabAPI

/// Core test 5 in docs/testing.md, on the Swift side. The Go tests in package api write every
/// message a hab of revision N sends to api/testdata/rev<N>/, with every field set and extra
/// x_unknown_* fields in every object. These types must decode every file of every revision. For
/// the current revision, each value must also survive a round trip: re-encoding it gives the file
/// less its extra fields. A file named <message>@<n>.json is a later field set of the message;
/// an older field set lacks only optional fields, so its round trip leaves them absent.
final class GoldenTests: XCTestCase {
    /// api/testdata in the repo, found from this file's path so plain `swift test` works.
    static let goldenRoot = URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent() // HabAPITests
        .deletingLastPathComponent() // Tests
        .deletingLastPathComponent() // HabAPI
        .deletingLastPathComponent() // swift
        .deletingLastPathComponent() // repo root
        .appendingPathComponent("api")
        .appendingPathComponent("testdata")

    static let unknownPrefix = "x_unknown_"

    func testGoldensDecode() throws {
        var checked = 0
        for revision in 1...HabAPI.revision {
            let dir = Self.goldenRoot.appendingPathComponent("rev\(revision)")
            guard let files = try? FileManager.default.contentsOfDirectory(atPath: dir.path) else {
                if revision >= HabAPI.minRevision {
                    XCTFail("no golden messages in \(dir.path)")
                }
                continue
            }
            for file in files.sorted() where file.hasSuffix(".json") {
                let data = try Data(contentsOf: dir.appendingPathComponent(file))
                try check(
                    name: Self.messageName(file), data: data,
                    roundTrip: revision == HabAPI.revision, label: "rev\(revision)/\(file)")
                checked += 1
            }
        }
        XCTAssertGreaterThan(checked, 0, "no golden messages under \(Self.goldenRoot.path)")
    }

    /// The message a golden file holds: its name less ".json" and any "@<n>" field set number.
    static func messageName(_ file: String) -> String {
        let base = String(file.dropLast(".json".count))
        return base.split(separator: "@", maxSplits: 1).first.map(String.init) ?? base
    }

    /// Readers for every golden that is not an event, keyed by the file name without .json.
    static func readers() -> [String: (Data) throws -> any Encodable] {
        var r: [String: (Data) throws -> any Encodable] = [:]
        func add<T: Codable>(_ name: String, _: T.Type) {
            r[name] = { try JSONDecoder().decode(T.self, from: $0) }
        }
        func result<P: Codable & Sendable, R: Codable & Sendable>(_ m: HabMethod<P, R>) {
            add("result." + m.name, R.self)
        }
        result(HabMethods.habHello)
        result(HabMethods.habPing)
        result(HabMethods.structureGet)
        result(HabMethods.eventsSubscribe)
        result(HabMethods.paneInput)
        result(HabMethods.dataTicket)
        result(HabMethods.paneTake)
        result(HabMethods.paneRequest)
        result(HabMethods.paneDisown)
        result(HabMethods.paneSetDesiredSize)
        result(HabMethods.paneRedraw)
        result(HabMethods.devicesPair)
        result(HabMethods.devicesList)
        result(HabMethods.devicesRevoke)
        result(HabMethods.sessionCreate)
        result(HabMethods.sessionResume)
        result(HabMethods.paneClose)
        result(HabMethods.sessionRename)
        result(HabMethods.sessionClose)
        result(HabMethods.sessionFocus)
        result(HabMethods.sessionInterrupt)
        result(HabMethods.sessionSend)
        result(HabMethods.approvalAnswer)
        result(HabMethods.chatPage)
        result(HabMethods.commandList)
        result(HabMethods.commandOutput)
        result(HabMethods.attachmentPut)
        result(HabMethods.attachmentGet)
        result(HabMethods.pushRegister)
        result(HabMethods.pushRegisterActivity)
        result(HabMethods.pushUnregister)
        result(HabMethods.usageList)
        result(HabMethods.habStats)
        result(HabMethods.peersList)
        result(HabMethods.peersAccept)
        result(HabMethods.habPermissions)
        add("notification." + HabNotification.heartbeat, Heartbeat.self)
        add("notification." + HabNotification.closing, Closing.self)
        add("frame.hello", DataHello.self)
        add("frame.welcome", DataWelcome.self)
        add("frame.reject", DataReject.self)
        add("frame.resize", ResizeFrame.self)
        add("frame.exited", ExitedFrame.self)
        add("frame.credit", CreditFrame.self)
        add("error.data", ErrorData.self)
        return r
    }

    private func check(name: String, data: Data, roundTrip: Bool, label: String) throws {
        let golden = try JSONDecoder().decode(JSONValue.self, from: data)
        let isEventFile = name.hasPrefix("event.")
        if isEventFile || name == "notification." + HabNotification.event {
            let event: Event
            do {
                event = try JSONDecoder().decode(Event.self, from: data)
            } catch {
                return XCTFail("\(label): \(error)")
            }
            if isEventFile {
                let type = String(name.dropFirst("event.".count))
                XCTAssertEqual(event.type, type, label)
                let decoded: HabEvent
                do {
                    decoded = try HabEvent(event)
                } catch {
                    return XCTFail("\(label): data: \(error)")
                }
                if case .unknown = decoded {
                    return XCTFail("\(label): event type \(type) decoded as unknown")
                }
                if roundTrip {
                    guard let payload = Mirror(reflecting: decoded).children.first?.value as? any Encodable else {
                        return XCTFail("\(label): no payload in \(decoded)")
                    }
                    guard case .object(let fields) = golden, let goldenData = fields["data"] else {
                        return XCTFail("\(label): no data")
                    }
                    try assertSame(payload, Self.strip(goldenData), "\(label) data")
                }
            }
            if roundTrip {
                // Event.data is any JSON value, so it keeps its extra fields.
                try assertSame(event, Self.strip(golden, keep: "data"), label)
            }
            return
        }
        guard let read = Self.readers()[name] else {
            return XCTFail("\(label): no Swift type reads \(name)")
        }
        let value: any Encodable
        do {
            value = try read(data)
        } catch {
            return XCTFail("\(label): \(error)")
        }
        if roundTrip {
            try assertSame(value, Self.strip(golden), label)
        }
    }

    private func assertSame(_ value: any Encodable, _ want: JSONValue, _ label: String) throws {
        let got = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(value))
        XCTAssertEqual(got, want, "\(label): a value did not survive the round trip")
    }

    /// Nested fields that hold any JSON value. Like an event's data, they keep extra fields.
    static let rawFields: Set<String> = ["input"]  // PermissionRequest.input, ChatTool.input

    /// Removes the x_unknown_* fields from every object. Below the key `keep`, or a key in
    /// `rawFields`, it leaves the value as it is.
    static func strip(_ v: JSONValue, keep: String? = nil) -> JSONValue {
        switch v {
        case .object(let fields):
            var out: [String: JSONValue] = [:]
            for (k, x) in fields where !k.hasPrefix(unknownPrefix) {
                out[k] = (k == keep || rawFields.contains(k)) ? x : strip(x)
            }
            return .object(out)
        case .array(let items):
            return .array(items.map { strip($0) })
        default:
            return v
        }
    }
}
