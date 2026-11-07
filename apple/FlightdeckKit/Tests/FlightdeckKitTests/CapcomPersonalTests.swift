import FlightdeckShared
import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

// Capcom's personal tools in the app (capcom-service.md 8.1, 8.3, 8.5; stage A5): the device
// context reporter, the calendar and reminder cards' claim-then-write, the push's Add, and
// the Apple Weather mark. Fakes stand in for CoreLocation, EventKit and the write.

@MainActor
final class FakeLocation: DeviceLocationSource {
    var authorization: ShareAuth
    var onFix: ((DeviceLocation) -> Void)?
    var onAuthorization: (() -> Void)?
    var starts = 0
    var refreshes = 0
    var stops = 0
    var asked = 0
    init(_ a: ShareAuth) { authorization = a }
    func requestPermission() { asked += 1 }
    func start() { starts += 1 }
    func refresh() { refreshes += 1 }
    func stop() { stops += 1 }
}

@MainActor
final class FakeCalendar: DeviceCalendarSource {
    var authorization: ShareAuth
    var list: [DeviceEvent] = []
    var windows: [(Date, Date, [String]?)] = []
    init(_ a: ShareAuth) { authorization = a }
    /// What the system answers when asked.
    var answer: ShareAuth = .granted
    func requestAccess() async -> Bool {
        if authorization == .notDetermined { authorization = answer }
        return authorization == .granted
    }
    func calendars() -> [DeviceCalendarInfo] { [DeviceCalendarInfo(id: "home", title: "Home")] }
    func events(from: Date, to: Date, calendars: [String]?) -> [DeviceEvent] {
        windows.append((from, to, calendars))
        return list
    }
}

@MainActor
final class FakeWriter: CapcomEventWriter {
    var canWrite = true
    var writes: [CapcomEventDraft] = []
    var fail: String?
    /// Runs at each write, before it lands: the test checks the hab took the answer first.
    var before: (() async -> Void)?
    func write(_ d: CapcomEventDraft) async throws {
        await before?()
        if let fail { throw CapcomWriteError(fail) }
        writes.append(d)
    }
}

@MainActor
final class CapcomPersonalTests: XCTestCase {
    private let fix = DeviceLocation(accuracy: 30, country: "DE", lat: 52.52, lon: 13.40, place: "Mitte, Berlin", time: "2026-11-07T09:00:00Z")

    private func defaults() -> UserDefaults {
        let name = "flightdeck.tests.capcom-personal"
        let d = UserDefaults(suiteName: name)!
        d.removePersistentDomain(forName: name)
        return d
    }

    private func reporter(location: ShareAuth = .granted, calendar: ShareAuth = .granted) -> (DeviceContextReporter, FakeLocation, FakeCalendar, () -> [DeviceContextPutParams]) {
        let l = FakeLocation(location)
        let c = FakeCalendar(calendar)
        let tz = TimeZone(identifier: "Europe/Berlin")!
        let r = DeviceContextReporter(prefs: CapcomPrefs(defaults: defaults()), location: l, calendar: c,
                                      now: { PushMeta.parseDate("2026-11-07T10:30:00Z")! }, timeZone: { tz })
        var puts: [DeviceContextPutParams] = []
        r.send = { p in puts.append(p); return true }
        return (r, l, c, { puts })
    }

    // MARK: - Device context

    func testTimeZoneWithEveryPutAndLocationOnlyWhenSharing() async {
        let (r, l, _, puts) = reporter()
        await r.appOpened()
        XCTAssertEqual(l.starts, 1, "significant-change updates start")
        XCTAssertEqual(l.refreshes, 1, "a fresh fix while the app is open")
        await r.locationUpdated(fix)
        let p = puts()
        XCTAssertEqual(p.count, 2)
        XCTAssertTrue(p.allSatisfy { $0.tz == "Europe/Berlin" }, "the time zone goes with every put")
        XCTAssertNil(p[0].location, "no fix yet")
        XCTAssertEqual(p[1].location, fix)
        XCTAssertEqual(p[1].location?.country, "DE")
        XCTAssertNotNil(p[1].calendar, "the calendar goes with each location update")
        XCTAssertNil(p[1].locationOff)

        // Sharing off: location_off with the reason, and the fix is dropped.
        r.prefs.shareLocation = false
        await r.sharesChanged()
        await r.locationUpdated(fix)
        XCTAssertEqual(l.stops, 1)
        XCTAssertEqual(puts().count, 3, "a fix while sharing is off sends nothing")
        XCTAssertEqual(puts().last?.locationOff, "off")
        XCTAssertNil(puts().last?.location)
        XCTAssertEqual(puts().last?.tz, "Europe/Berlin")

        // On again: the old fix is gone; nothing goes until a new one.
        r.prefs.shareLocation = true
        await r.sharesChanged()
        XCTAssertNil(puts().last?.location)
        XCTAssertNil(puts().last?.locationOff)
    }

    func testDeniedSendsNoPermission() async {
        let (r, l, _, puts) = reporter(location: .denied, calendar: .denied)
        await r.appOpened()
        await r.locationUpdated(fix)
        XCTAssertEqual(l.starts, 0)
        XCTAssertEqual(puts().count, 1)
        XCTAssertEqual(puts()[0].locationOff, "no_permission")
        XCTAssertEqual(puts()[0].calendarOff, "no_permission")
        XCTAssertNil(puts()[0].location)
        XCTAssertNil(puts()[0].calendar)
        XCTAssertEqual(puts()[0].tz, "Europe/Berlin")
    }

    func testCalendarDaysWindowAndOff() async throws {
        let (r, _, c, puts) = reporter()
        c.list = [DeviceEvent(calendar: "Home", end: "2026-11-08T11:00:00Z", start: "2026-11-08T10:00:00Z", title: "Dentist")]
        r.prefs.calendarDays = 3
        r.prefs.calendarIDs = ["home"]
        await r.appOpened()
        let cal = try XCTUnwrap(puts().last?.calendar)
        let from = try XCTUnwrap(PushMeta.parseDate(cal.from))
        let to = try XCTUnwrap(PushMeta.parseDate(cal.to))
        XCTAssertEqual(to.timeIntervalSince(from), 3 * 86400, accuracy: 3600, "a 3-day window")
        XCTAssertEqual(cal.from, "2026-11-06T23:00:00Z", "from the start of today in the device's zone")
        XCTAssertEqual(cal.events.map(\.title), ["Dentist"])
        XCTAssertEqual(c.windows.last?.2, ["home"], "only the picked calendars")
        // The preference stays in 1 to 31.
        r.prefs.calendarDays = 40
        XCTAssertEqual(r.prefs.calendarDays, 31)
        r.prefs.calendarDays = 0
        XCTAssertEqual(r.prefs.calendarDays, 1)
        r.prefs.shareCalendar = false
        await r.sharesChanged()
        XCTAssertEqual(puts().last?.calendarOff, "off")
        XCTAssertNil(puts().last?.calendar)
    }

    func testPreferenceDefaults() {
        let p = CapcomPrefs(defaults: defaults())
        XCTAssertTrue(p.shareLocation)
        XCTAssertTrue(p.shareCalendar)
        XCTAssertEqual(p.calendarDays, 7)
        XCTAssertNil(p.calendarIDs)
        XCTAssertFalse(p.locationAsked)
    }

    func testFirstOpenAsksOnce() async {
        let (r, l, _, _) = reporter(location: .notDetermined, calendar: .notDetermined)
        XCTAssertTrue(r.shouldAsk)
        await r.askPermissions()
        XCTAssertEqual(l.asked, 1)
        XCTAssertFalse(r.shouldAsk)
    }

    func testPutGoesToTheHomeHabOnly() async throws {
        // Both run a Capcom engine and list device-context; only bench holds the mark.
        let home = await sampleHab("bench")
        let other = await sampleHab("attic")
        let third = await sampleHab("cellar")
        for h in [home, other] {
            await h.setCapability("capcom", true)
            await h.setCapcomService(true)
            await h.setDeviceContext(true)
            await h.setCapcomHome(assignment: 2, home: home.deviceID)
        }
        let f = Fleet()
        f.roundDefaults = defaults()
        let (r, _, _, _) = reporter()
        r.send = { [weak f] p in await f?.sendDeviceContextLive(p) ?? false }
        f.capcom.deviceContext = r
        for h in [other, third, home] {
            f.add(HabConnection(id: h.hostID, transport: h.transport(), timing: fastTiming()), label: h.hostID)
        }
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        await eventually("home known") { f.capcom.entry == .ready(habID: "bench") }
        await eventually("a put on the home") { await !home.deviceContextPuts.isEmpty }
        let puts = await home.deviceContextPuts
        XCTAssertEqual(puts.last?.tz, "Europe/Berlin")
        let none = await other.deviceContextPuts
        XCTAssertTrue(none.isEmpty, "a capable hab that is not the home gets nothing")
        let none3 = await third.deviceContextPuts
        XCTAssertTrue(none3.isEmpty)
        XCTAssertEqual(f.deviceContextHome, "bench", "the background put goes to the home")
        await f.setActive(false)
    }

    func testDeviceLabelIsStableAndDistinct() {
        let d = defaults()
        let a = DeviceLabel.label(kind: "iPhone", defaults: d)
        XCTAssertNotNil(a.wholeMatch(of: #/iPhone [0-9A-F]{4}/#), a)
        XCTAssertEqual(DeviceLabel.label(kind: "iPhone", defaults: d), a, "the same label at every start")
        XCTAssertEqual(DeviceLabel.label(kind: "iPad", defaults: d).suffix(4), a.suffix(4))
        d.removePersistentDomain(forName: "flightdeck.tests.capcom-personal")
        var seen: Set<String> = []
        for _ in 0..<20 {
            let e = UserDefaults(suiteName: "flightdeck.tests.capcom-personal")!
            e.removePersistentDomain(forName: "flightdeck.tests.capcom-personal")
            seen.insert(DeviceLabel.label(kind: "iPad", defaults: e))
        }
        XCTAssertGreaterThan(seen.count, 1, "a new device gets its own label")
    }

    func testFirstOpenAsksForCalendarWhenLocationWasAnswered() async {
        let (r, l, _, puts) = reporter(location: .granted, calendar: .notDetermined)
        XCTAssertTrue(r.shouldAsk, "calendar access is still to ask")
        await r.askPermissions()
        XCTAssertEqual(l.asked, 0, "location was answered already")
        XCTAssertFalse(r.shouldAsk)
        XCTAssertNotNil(puts().last?.calendar, "the snapshot goes once access is granted")
        // Both answered: nothing to ask.
        let (r2, _, _, _) = reporter(location: .denied, calendar: .granted)
        XCTAssertFalse(r2.shouldAsk)
    }

    // MARK: - Cards

    private func cardHab() async -> FakeHab {
        let h = await sampleHab("bench")
        await h.setCapability("capcom", true)
        await h.setCapcomService(true)
        await h.setSend(works: true)
        return h
    }

    private func open(_ h: FakeHab, writer: FakeWriter) async throws -> (Fleet, ChatModel, CapcomThread) {
        let f = Fleet()
        f.add(HabConnection(id: h.hostID, transport: h.transport(), timing: fastTiming(), clientLabel: "phone"), label: h.hostID)
        f.capcom.cards.writer = writer
        f.capcom.cards.label = "phone"
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        await eventually("home read") { f.capcom.entry != .loading && f.capcom.reading.isEmpty }
        await f.capcom.openStanding()
        let m = try XCTUnwrap(f.capcom.standing)
        m.start()
        await eventually("service context") { m.thread != nil && m.context.online }
        let t = try XCTUnwrap(m.thread)
        await eventually("thread loaded") { t.load == .ready }
        return (f, m, t)
    }

    private func calendarCard(_ start: String = "2027-03-01T18:00:00Z", _ end: String = "2027-03-01T19:30:00Z") -> CapcomCard {
        CapcomCard(choices: ["add", "dismiss"], event: CapcomCardEvent(end: end, location: "Cafe Rot", notes: "Bring the book", start: start, title: "Dinner with Ana"),
                   type: "calendar")
    }

    func testAddAnswersBeforeTheWriteAndWritesOnce() async throws {
        let a = await cardHab()
        let w = FakeWriter()
        let (f, m, t) = try await open(a, writer: w)
        var answersAtWrite: [Int] = []
        w.before = { answersAtWrite.append(await a.capcomCardAnswers.count) }
        let card = await a.addCapcomItem(kind: "card", author: "capcom", text: "Add dinner?", card: calendarCard())
        await eventually("card") { t.items.contains { $0.id == card.id } }
        XCTAssertTrue(w.writes.isEmpty, "nothing is written without a tap")
        XCTAssertEqual(CapcomCardAdder.buttons(card, now: Date()), [.add, .edit, .dismiss])

        let claim = try XCTUnwrap(t.cardClaim)
        let o = await f.capcom.cards.add(card, claim: claim, applied: t.apply)
        XCTAssertEqual(o, .added)
        XCTAssertEqual(answersAtWrite, [1], "the answer reached the hab before the write")
        XCTAssertEqual(w.writes.count, 1)
        XCTAssertEqual(w.writes.first?.title, "Dinner with Ana")
        XCTAssertEqual(w.writes.first?.location, "Cafe Rot")
        XCTAssertEqual(w.writes.first?.start, PushMeta.parseDate("2027-03-01T18:00:00Z"))
        let answers = await a.capcomCardAnswers
        XCTAssertEqual(answers, [CapcomCardAnswerParams(choice: "add", item: card.id)])
        let answered = try XCTUnwrap(t.items.first { $0.id == card.id }?.answer)
        XCTAssertEqual(CapcomCardAdder.answered(answered), "Added on phone")
        XCTAssertEqual(CapcomCardAdder.buttons(t.items.first { $0.id == card.id }!, now: Date()), [], "an answered card has no buttons")

        // A second tap on the stale copy: the hab says conflict, and nothing is written.
        let again = await f.capcom.cards.add(card, claim: claim)
        guard case .conflict = again else { return XCTFail("want conflict, got \(again)") }
        XCTAssertEqual(w.writes.count, 1, "written exactly once")
        m.stop()
        await f.setActive(false)
    }

    func testConflictAndDismissWriteNothing() async throws {
        let a = await cardHab()
        let w = FakeWriter()
        let (f, m, t) = try await open(a, writer: w)
        let card = await a.addCapcomItem(kind: "card", author: "capcom", text: "Add dinner?", card: calendarCard())
        await eventually("card") { t.items.contains { $0.id == card.id } }
        // Another device answered first.
        var other = card
        other.answer = CapcomAnswer(by: "mac", choice: "add", time: "2026-11-07T10:00:00Z")
        await a.putCapcomItem(other)
        let o = await f.capcom.cards.add(card, claim: try XCTUnwrap(t.cardClaim))
        guard case .conflict = o else { return XCTFail("want conflict, got \(o)") }
        XCTAssertTrue(w.writes.isEmpty, "conflict writes nothing")
        await eventually("other device's answer") { t.items.first { $0.id == card.id }?.answer?.by == "mac" }
        XCTAssertEqual(CapcomCardAdder.answered(t.items.first { $0.id == card.id }!.answer!), "Added on mac")

        let r = await a.addCapcomItem(kind: "card", author: "capcom", text: "Remind you?",
                                      card: CapcomCard(choices: ["add", "dismiss"], event: CapcomCardEvent(due: "2027-01-05", title: "Renew passport"), type: "reminder"))
        await eventually("reminder") { t.items.contains { $0.id == r.id } }
        let d = await f.capcom.cards.dismiss(r, claim: try XCTUnwrap(t.cardClaim), applied: t.apply)
        XCTAssertEqual(d, .dismissed)
        XCTAssertTrue(w.writes.isEmpty, "Dismiss writes nothing")
        XCTAssertEqual(CapcomCardAdder.answered(t.items.first { $0.id == r.id }!.answer!), "Dismissed")
        m.stop()
        await f.setActive(false)
    }

    func testEditSavesAsAddInTheSameOrder() async throws {
        let a = await cardHab()
        let w = FakeWriter()
        let (f, m, t) = try await open(a, writer: w)
        var answersAtWrite: [Int] = []
        w.before = { answersAtWrite.append(await a.capcomCardAnswers.count) }
        let card = await a.addCapcomItem(kind: "card", author: "capcom", text: "Add dinner?", card: calendarCard())
        await eventually("card") { t.items.contains { $0.id == card.id } }
        var edited = try XCTUnwrap(CapcomEventDraft.of(card))
        edited.title = "Dinner with Ana and Ben"
        let o = await f.capcom.cards.add(card, edited: edited, claim: try XCTUnwrap(t.cardClaim), applied: t.apply)
        XCTAssertEqual(o, .added)
        XCTAssertEqual(answersAtWrite, [1])
        XCTAssertEqual(w.writes.map(\.title), ["Dinner with Ana and Ben"])
        m.stop()
        await f.setActive(false)
    }

    func testPastCardHidesAdd() async throws {
        let a = await cardHab()
        let w = FakeWriter()
        let (f, m, t) = try await open(a, writer: w)
        let card = await a.addCapcomItem(kind: "card", author: "capcom", text: "Add lunch?",
                                         card: calendarCard("2026-01-01T12:00:00Z", "2026-01-01T13:00:00Z"))
        await eventually("card") { t.items.contains { $0.id == card.id } }
        XCTAssertTrue(try XCTUnwrap(CapcomEventDraft.of(card)).isPast(Date()))
        XCTAssertEqual(CapcomCardAdder.buttons(card, now: Date()), [.dismiss], "Past: no Add, no Edit")
        // The hab refuses add once the event has ended; nothing is written.
        let o = await f.capcom.cards.add(card, claim: try XCTUnwrap(t.cardClaim))
        guard case .refused = o else { return XCTFail("want refused, got \(o)") }
        XCTAssertTrue(w.writes.isEmpty)
        // An all-day event is past only after its last day.
        let allDay = CapcomEventDraft.of(CapcomCardEvent(allDay: true, end: "2026-11-07", start: "2026-11-06", title: "Fair", tz: "Europe/Berlin"), kind: .calendar)
        XCTAssertFalse(allDay.isPast(PushMeta.parseDate("2026-11-07T22:00:00Z")!))
        XCTAssertTrue(allDay.isPast(PushMeta.parseDate("2026-11-07T23:00:00Z")!))
        // A reminder never shows Past.
        XCTAssertFalse(CapcomEventDraft.of(CapcomCardEvent(due: "2020-01-01", title: "Old"), kind: .reminder).isPast(Date()))
        m.stop()
        await f.setActive(false)
    }

    func testWriteFailureOffersRetryOnTheAnsweringDevice() async throws {
        let a = await cardHab()
        let w = FakeWriter()
        w.fail = "Calendar access is off."
        let (f, m, t) = try await open(a, writer: w)
        let card = await a.addCapcomItem(kind: "card", author: "capcom", text: "Add dinner?", card: calendarCard())
        await eventually("card") { t.items.contains { $0.id == card.id } }
        let o = await f.capcom.cards.add(card, claim: try XCTUnwrap(t.cardClaim), applied: t.apply)
        XCTAssertEqual(o, .writeFailed("Calendar access is off."))
        let answered = try XCTUnwrap(t.items.first { $0.id == card.id })
        XCTAssertEqual(f.capcom.cards.failures[card.id], "Calendar access is off.")
        XCTAssertTrue(f.capcom.cards.canRetry(answered))
        // Another device's label: no Retry here.
        f.capcom.cards.label = "mac"
        XCTAssertFalse(f.capcom.cards.canRetry(answered))
        f.capcom.cards.label = "phone"
        w.fail = nil
        let r = await f.capcom.cards.retry(answered)
        XCTAssertEqual(r, .added)
        XCTAssertEqual(w.writes.count, 1)
        let answers = await a.capcomCardAnswers
        XCTAssertEqual(answers.count, 1, "Retry writes again without a second answer")
        XCTAssertNil(f.capcom.cards.failures[card.id])
        m.stop()
        await f.setActive(false)
    }

    func testLostClaimReplyOffersRetry() async throws {
        let a = await cardHab()
        let w = FakeWriter()
        let (f, m, t) = try await open(a, writer: w)
        let card = await a.addCapcomItem(kind: "card", author: "capcom", text: "Add dinner?", card: calendarCard())
        await eventually("card") { t.items.contains { $0.id == card.id } }
        let real = try XCTUnwrap(t.cardClaim)
        // The hab takes the answer, then the reply is lost on the way back.
        let o = await f.capcom.cards.add(card, claim: { p in
            _ = try await real(p)
            throw ConnectionClosed(detail: nil)
        })
        guard case .unconfirmed = o else { return XCTFail("want unconfirmed, got \(o)") }
        XCTAssertTrue(w.writes.isEmpty, "nothing written without a confirmed answer")
        XCTAssertEqual(f.capcom.cards.failures[card.id]?.hasPrefix("Not confirmed"), true)
        await eventually("answered by this device") { t.items.first { $0.id == card.id }?.answer?.by == "phone" }
        let answered = try XCTUnwrap(t.items.first { $0.id == card.id })
        XCTAssertTrue(f.capcom.cards.canRetry(answered))
        let r = await f.capcom.cards.retry(answered)
        XCTAssertEqual(r, .added)
        XCTAssertEqual(w.writes.count, 1)
        let answers = await a.capcomCardAnswers
        XCTAssertEqual(answers.count, 1)
        m.stop()
        await f.setActive(false)
    }

    func testNoWriterOrNoEntitlementWritesNothing() async throws {
        let w = FakeWriter()
        w.canWrite = false
        let adder = CapcomCardAdder(writer: w, label: "mac")
        let item = CapcomItem(author: CapcomAuthor(kind: .capcom), card: calendarCard(), id: "c1", kind: CapcomItemKind(rawValue: "card"), time: "2026-11-07T10:00:00Z")
        var claims = 0
        let o = await adder.add(item) { _ in claims += 1; throw CapcomWriteError("unused") }
        XCTAssertEqual(o, .none)
        XCTAssertEqual(claims, 0, "no answer without a way to write")
        XCTAssertFalse(adder.canWrite)
    }

    func testPushAddClaimsThenWrites() async throws {
        let meta = PushMeta(kind: "capcom", hab: "bench", session: "s1", item: "c0000000000000001")
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.capcomCardAddAction, meta: meta), .capcomCardAdd(host: "bench", item: "c0000000000000001"))
        var noItem = meta
        noItem.item = nil
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.capcomCardAddAction, meta: noItem), .open(noItem))
        // The item id rides in the push's flightdeck object.
        let back = PushMeta(userInfo: [PushMeta.userInfoKey: meta.userInfoValue])
        XCTAssertEqual(back?.item, "c0000000000000001")

        let a = await cardHab()
        let w = FakeWriter()
        let (f, m, t) = try await open(a, writer: w)
        var answersAtWrite: [Int] = []
        w.before = { answersAtWrite.append(await a.capcomCardAnswers.count) }
        let card = await a.addCapcomItem(kind: "card", author: "capcom", text: "Add dinner?", card: calendarCard())
        await eventually("card") { t.items.contains { $0.id == card.id } }
        let claim = try XCTUnwrap(t.cardClaim)
        let o = await f.capcom.cards.addFromPush(item: card.id, claim: claim)
        XCTAssertEqual(o, .added)
        XCTAssertEqual(answersAtWrite, [1])
        XCTAssertEqual(w.writes.map(\.title), ["Dinner with Ana"])
        let again = await f.capcom.cards.addFromPush(item: card.id, claim: claim)
        guard case .conflict = again else { return XCTFail("want conflict, got \(again)") }
        XCTAssertEqual(w.writes.count, 1)
        m.stop()
        await f.setActive(false)
    }

    // MARK: - Weather attribution

    func testWeatherMarkShowsOnlyWhereCarried() {
        let url = "https://weatherkit.apple.com/legal-attribution.html"
        let with = CapcomItem(attribution: [CapcomAttribution(source: "weather", text: "Apple Weather", url: url)],
                              author: CapcomAuthor(kind: .capcom), id: "c1", kind: CapcomItemKind(rawValue: "message"), text: "Rain after 4.", time: "2026-11-07T10:00:00Z")
        let without = CapcomItem(author: CapcomAuthor(kind: .capcom), id: "c2", kind: CapcomItemKind(rawValue: "message"), text: "Hello.", time: "2026-11-07T10:00:00Z")
        guard case .capcom(_, _, _, let marks) = CapcomRow.of(with) else { return XCTFail("not a Capcom message") }
        XCTAssertEqual(marks, [CapcomMark(symbol: "apple.logo", text: "Weather", url: URL(string: url))])
        guard case .capcom(_, _, _, let none) = CapcomRow.of(without) else { return XCTFail("not a Capcom message") }
        XCTAssertEqual(none, [])
    }
}
