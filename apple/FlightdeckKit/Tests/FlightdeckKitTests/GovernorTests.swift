import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The usage governor in the app (capcom-service.md 10.1): the pacing line on the Capcom screen
/// and the usage sheet from a fake hab's catalog, for each basis and pace; nothing from a hab
/// whose catalog has no governor section; and "Send now" on the Capcom thread.
@MainActor
final class GovernorTests: XCTestCase {
    private var utc: Calendar {
        var c = Calendar(identifier: .gregorian)
        c.timeZone = TimeZone(identifier: "UTC")!
        return c
    }
    private let locale = Locale(identifier: "en_GB")
    /// 2026-11-18 10:00 UTC.
    private let now = Date(timeIntervalSince1970: 1_794_996_000)
    private let today = "2026-11-18T15:04:00Z"
    private let tomorrow = "2026-11-19T09:30:00Z"

    private func serviceHab(_ host: String, governor: CatalogGovernor?) async -> FakeHab {
        let h = await sampleHab(host)
        await h.setCapability("capcom", true)
        await h.setCapcomService(true)
        await h.setSend(works: true)
        await h.setGovernor(governor)
        return h
    }

    private func fleet(_ habs: [FakeHab]) async -> Fleet {
        let f = Fleet()
        for h in habs {
            f.add(HabConnection(id: h.hostID, transport: h.transport(), timing: fastTiming()), label: h.hostID)
        }
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        await eventually("home read") {
            f.capcom.entry != .loading && f.capcom.reading.isEmpty && habs.allSatisfy { f.capcom.beliefs[$0.hostID] != nil }
        }
        return f
    }

    private func line(_ f: Fleet) -> String? {
        f.capcom.governorLine(now: now, calendar: utc, locale: locale)
    }

    private func rows(_ f: Fleet) -> [GovernorRow] {
        f.capcom.governorRows(now: now, calendar: utc, locale: locale)
    }

    private func plan(_ pace: String, event: String = "ok", until: String? = nil, eventUntil: String? = nil, id: String = "capcom") -> GovernorConsumer {
        GovernorConsumer(basis: "plan", eventPace: event, eventUntil: eventUntil, id: id, inputTokens: 900, outputTokens: 300,
                         pace: pace, sharePct: 30, until: until, usedPct: 40)
    }

    private func usd(_ pace: String, event: String = "ok", until: String? = nil, eventUntil: String? = nil) -> GovernorConsumer {
        GovernorConsumer(basis: "usd", capCents: 500, eventPace: event, eventUntil: eventUntil, id: "capcom", pace: pace,
                         spentCents: 612, until: until)
    }

    func testPacingLineForEachBasisAndPace() async throws {
        let a = await serviceHab("bench", governor: CatalogGovernor(consumers: [plan("ok")]))
        let f = await fleet([a])
        XCTAssertEqual(f.capcom.entry, .ready(habID: "bench"))
        XCTAssertNil(line(f), "ok on a reading shows nothing")

        let cases: [(GovernorConsumer, String?)] = [
            (plan("ok"), nil),
            (plan("small"), "Small model for your messages · 40% of the week used, share 30%"),
            (plan("small", event: "wait", eventUntil: today),
             "Small model for your messages; Capcom's own turns wait until 15:04 · 40% of the week used, share 30%"),
            (plan("ok", event: "wait", eventUntil: tomorrow), "Capcom's own turns wait until Thu 09:30 · 40% of the week used, share 30%"),
            // An older hab's Capcom, whose user turns still wait.
            (plan("wait", event: "wait", until: today, eventUntil: today), "Pacing usage until 15:04 · 40% of the week used, share 30%"),
            (usd("ok"), nil),
            (usd("small"), "Small model for your messages · $6.12 of $5.00 today"),
            (usd("ok", event: "wait", eventUntil: today), "Capcom's own turns wait until 15:04 · $6.12 of $5.00 today"),
            (GovernorConsumer(basis: "none", eventPace: "ok", id: "capcom", inputTokens: 11_000, outputTokens: 1_300, pace: "ok"),
             "Unpaced: 12.3k tokens today"),
        ]
        for (k, want) in cases {
            await a.setGovernor(CatalogGovernor(consumers: [k]))
            await a.announceCatalog()
            await eventually("governor \(k.basis) \(k.pace) \(k.eventPace)") {
                f.capcom.governors["bench"]?.consumers == [k]
            }
            XCTAssertEqual(line(f), want, "\(k.basis) pace \(k.pace) event \(k.eventPace)")
            XCTAssertEqual(rows(f).map(\.text), want.map { [$0] } ?? [], "the usage sheet shows the same line")
        }
        await f.setActive(false)
    }

    func testUsageSheetRowsByHabAndConsumer() async throws {
        let a = await serviceHab("bench", governor: CatalogGovernor(consumers: [
            plan("small"), plan("ok", id: "group:g1"), plan("wait", until: today, id: "group:g2"),
            plan("small", event: "wait", eventUntil: today, id: "group:g3"),
        ]))
        let f = await fleet([a])
        let r = rows(f)
        XCTAssertEqual(r.map(\.consumer), ["capcom", "group:g2", "group:g3"], "a consumer at ok has no row")
        XCTAssertEqual(r.map(\.title), ["Capcom", "group:g2", "group:g3"])
        XCTAssertEqual(r.first?.habLabel, "bench")
        XCTAssertEqual(r[1].text, "Pacing usage until 15:04 · 40% of the week used, share 30%", "a group's wait")
        XCTAssertEqual(r[2].text, "Small model; event turns wait until 15:04 · 40% of the week used, share 30%")
        XCTAssertEqual(line(f), "Small model for your messages · 40% of the week used, share 30%", "the Capcom screen shows the capcom consumer only")
        await f.setActive(false)
    }

    /// Capcom's four pace rows (capcom-service.md 19.1): preferences on every hab, under Usage,
    /// with the hab's ranges.
    func testPaceSettingsRows() throws {
        let R = SettingsRegistry.self
        let rows: [(String, SettingKind)] = [
            (R.capcomPaceDailyShare, .integer(1...100, unit: "%")),
            (R.capcomPaceSmallAt, .integer(10...500, unit: "% of share")),
            (R.capcomPaceEvents, .bool),
            (R.capcomPaceEventStop, .integer(10...500, unit: "% of share")),
        ]
        for (id, kind) in rows {
            let d = try XCTUnwrap(R.def(id), id)
            XCTAssertEqual(d.kind, kind, id)
            XCTAssertEqual(d.store, .everyHab, id)
            XCTAssertEqual(d.area, .usage, id)
            XCTAssertFalse(d.isLevelled, id)
            XCTAssertTrue(d.built, id)
        }
    }

    func testHabWithoutGovernorSectionShowsNothing() async throws {
        let a = await serviceHab("bench", governor: nil)
        let f = await fleet([a])
        XCTAssertNil(f.capcom.governors["bench"])
        XCTAssertNil(line(f))
        XCTAssertEqual(rows(f), [])

        // A section that goes away takes its line with it.
        await a.setGovernor(CatalogGovernor(consumers: [plan("small")]))
        await a.announceCatalog()
        await eventually("governor read") { self.line(f) != nil }
        await a.setGovernor(nil)
        await a.announceCatalog()
        await eventually("governor gone") { f.capcom.governors["bench"] == nil }
        XCTAssertNil(line(f))
        XCTAssertEqual(rows(f), [])
        await f.setActive(false)
    }

    func testSendNowSendsSendNow() async throws {
        let a = await serviceHab("bench", governor: CatalogGovernor(consumers: [plan("wait", until: today)]))
        let f = await fleet([a])
        await f.capcom.openStanding()
        let m = try XCTUnwrap(f.capcom.standing)
        m.start()
        await eventually("service context") { m.thread != nil && m.context.online && m.context.isCapcomService }

        m.send("plain send")
        await eventually("first send") { await a.sends.count == 1 }
        m.send("run it anyway", sendNow: true)
        await eventually("second send") { await a.sends.count == 2 }
        let sends = await a.sends
        XCTAssertEqual(sends.map(\.text), ["plain send", "run it anyway"])
        XCTAssertNil(sends[0].sendNow, "a plain send leaves send_now out")
        XCTAssertEqual(sends[1].sendNow, true)
        m.stop()
        await f.setActive(false)
    }
}
