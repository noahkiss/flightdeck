import Foundation
import FlightdeckShared
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The Capcom service's thread in the app (capcom-service.md 7.2 to 7.6, stage A3): typing,
/// delivered and read marks, the folded tool line, a divider with its summary, a photo sent and
/// shown, Stop, approval cards with "Allow this site", "Earlier threads" from another host,
/// the pinned proposals line, and the Reply from a notification.
@MainActor
final class CapcomThreadTests: XCTestCase {
    private func serviceHab(_ host: String) async -> FakeHab {
        let h = await sampleHab(host)
        await h.setCapability("capcom", true)
        await h.setCapcomService(true)
        await h.setSend(works: true)
        return h
    }

    private func fleet(_ habs: [FakeHab]) async -> Fleet {
        let f = Fleet()
        for h in habs {
            f.add(HabConnection(id: h.hostID, transport: h.transport(), timing: fastTiming()), label: h.hostID)
        }
        await f.setActive(true)
        await eventually("online") { f.board.habs.allSatisfy { $0.state.status == .online } }
        await eventually("home read") { f.capcom.entry != .loading && f.capcom.reading.isEmpty }
        return f
    }

    /// Opens the standing conversation and starts its thread, as the Capcom view does.
    private func open(_ f: Fleet) async throws -> (ChatModel, CapcomThread) {
        await f.capcom.openStanding()
        let m = try XCTUnwrap(f.capcom.standing)
        m.start()
        await eventually("service context") { m.thread != nil && m.context.online }
        let t = try XCTUnwrap(m.thread)
        await eventually("thread loaded") { t.load == .ready }
        return (m, t)
    }

    func testServiceSessionShowsTheThread() async throws {
        let a = await serviceHab("bench")
        let f = await fleet([a])
        let (m, t) = try await open(f)
        XCTAssertTrue(m.context.isCapcomService)
        XCTAssertNil(m.context.pane, "no pane: no terminal")
        XCTAssertEqual(m.gate, .now)
        XCTAssertEqual(t.items.count, 0)
        m.stop()
        await f.setActive(false)
    }

    func testDeliveredReadTypingToolsAndDivider() async throws {
        let a = await serviceHab("bench")
        let f = await fleet([a])
        let (m, t) = try await open(f)

        m.send("where should we eat")
        await eventually("delivered") { t.rows.contains { if case .user(_, "where should we eat", _, _, false) = $0 { return true }; return false } }
        let sent = try XCTUnwrap(t.items.last)
        await a.setCapcomTyping(true)
        await eventually("typing") { t.typing }
        await a.markCapcomRead(sent.id)
        await eventually("read") { t.rows.contains { if case .user(_, _, _, _, true) = $0 { return true }; return false } }

        // A turn's tool calls fold into one quiet line.
        let calls = [CapcomToolCall(name: "WebSearch", summary: "WebSearch: thai near me"),
                     CapcomToolCall(name: "WebSearch", summary: "WebSearch: thai open late"),
                     CapcomToolCall(name: "mcp__flightdeck__transcript_read", summary: "transcript_read: s1")]
        await a.addCapcomItem(kind: "tools", author: "capcom", tools: calls)
        await a.addCapcomItem(kind: "message", author: "capcom", text: "Try the place on 5th.")
        await a.setCapcomTyping(false)
        await eventually("reply") { t.items.count == 3 && !t.typing }
        XCTAssertEqual(t.rows[1], .tools(id: t.items[1].id, line: "searched the web ×2, read a transcript", calls: calls))
        XCTAssertEqual(t.rows[2], .capcom(id: t.items[2].id, text: "Try the place on 5th.", wroteFirst: false))

        // A divider shows the summary the fresh SDK session opened with; an event turn's
        // message is marked wrote first.
        await a.addCapcomItem(kind: "divider", author: "flightdeck", text: "New conversation", summary: "The user likes thai food.")
        await a.addCapcomItem(kind: "message", author: "capcom", text: "Your build failed.", event: true)
        await eventually("divider") { t.items.count == 5 }
        XCTAssertEqual(t.rows[3], .divider(id: t.items[3].id, label: "New conversation", summary: "The user likes thai food."))
        XCTAssertEqual(t.rows[4], .capcom(id: t.items[4].id, text: "Your build failed.", wroteFirst: true))
        m.stop()
        await f.setActive(false)
    }

    func testPhotoSentAndShown() async throws {
        let a = await serviceHab("bench")
        await a.setCapability("attachments", true)
        let f = await fleet([a])
        let (m, t) = try await open(f)
        let photo = DraftAttachment(name: "flyer.png", type: "image/png", data: Data([0x89, 0x50, 0x4e, 0x47, 1, 2, 3]))
        m.send("what is on this flyer", attachments: [photo])
        await eventually("photo message") {
            t.rows.contains { if case .user(_, "what is on this flyer", let att, _, _) = $0 { return att.count == 1 }; return false }
        }
        guard case .user(_, _, let att, _, _) = t.rows.last else { return XCTFail("no user row") }
        let sends = await a.sends
        XCTAssertEqual(sends.last?.attachments, att, "the thread item names the uploaded attachment")
        m.stop()
        await f.setActive(false)
    }

    func testStop() async throws {
        let a = await serviceHab("bench")
        let f = await fleet([a])
        let (m, t) = try await open(f)
        await m.interrupt()
        let n = await a.capcomInterrupts
        XCTAssertEqual(n, 1, "Stop is capcom.interrupt")
        XCTAssertNil(t.notice)
        await a.setCapcomInterruptUnavailable(true)
        await m.interrupt()
        XCTAssertEqual(m.notice?.hasPrefix("Stop failed: unavailable"), true)
        m.stop()
        await f.setActive(false)
    }

    func testApprovalCardAllowThisSite() async throws {
        let a = await serviceHab("bench")
        let f = await fleet([a])
        let (m, t) = try await open(f)
        let card = await a.addCapcomItem(kind: "card", author: "flightdeck", text: "Capcom wants to open example.com/menu",
                                         card: CapcomCard(choices: ["allow", "allow_site", "deny"], ref: "r1", type: "approval"))
        await eventually("card") { t.items.contains { $0.id == card.id } }
        XCTAssertTrue(t.isOpen(card))
        XCTAssertEqual(CapcomCardChoice.buttons(card), [.deny, .allowSite, .allow])
        await t.answer(card, .allowSite)
        await eventually("answered") { t.items.first { $0.id == card.id }?.answer?.choice == "allow_site" }
        let answers = await a.answers
        XCTAssertEqual(answers.last, ApprovalAnswerParams(choice: .allowSite, pane: CapcomThread.engineRoute, request: "r1", via: .app))
        // A second answer: the hab's reason shows.
        await t.answer(card, .deny)
        XCTAssertEqual(t.notice?.hasPrefix("conflict:"), true)
        // A card with no URL offers no "Allow this site".
        let plain = CapcomItem(author: CapcomAuthor(kind: CapcomAuthorKind(rawValue: "flightdeck")),
                               card: CapcomCard(choices: ["allow", "deny"], ref: "r2", type: "approval"),
                               id: "c9", kind: CapcomItemKind(rawValue: "card"), time: "2026-11-06T12:00:00Z")
        XCTAssertEqual(CapcomCardChoice.buttons(plain), [.deny, .allow])
        m.stop()
        await f.setActive(false)
    }

    func testAllowedSitesInSettings() async throws {
        let a = await serviceHab("bench")
        await a.setCapcomSites(["example.com", "example.org"])
        let f = await fleet([a])
        await f.capcom.loadSites()
        XCTAssertEqual(f.capcom.sites, ["example.com", "example.org"])
        await f.capcom.removeSite("example.com")
        XCTAssertEqual(f.capcom.sites, ["example.org"])
        let kept = await a.capcomSites
        XCTAssertEqual(kept, ["example.org"])
        // Already gone on the hab: it goes from the list too.
        await a.setCapcomSites([])
        await f.capcom.removeSite("example.org")
        XCTAssertEqual(f.capcom.sites, [])
        XCTAssertNil(f.capcom.sitesFailure)
        await f.setActive(false)
    }

    func testEarlierThreadsFromAnotherHost() async throws {
        let a = await serviceHab("bench")
        let b = await serviceHab("attic")
        // attic was the home once and kept its thread; bench is the home now.
        let benchID = a.deviceID
        await a.setCapcomHome(assignment: 2, home: benchID)
        await b.setCapcomHome(assignment: 2, home: benchID)
        await b.addCapcomItem(kind: "message", author: "capcom", text: "From before the move.")
        let f = await fleet([a, b])
        XCTAssertEqual(f.capcom.entry, .ready(habID: "bench"))
        await a.addCapcomItem(kind: "message", author: "capcom", text: "On the home.")
        await f.capcom.loadEarlier()
        XCTAssertEqual(f.capcom.earlier.map(\.label), ["attic"], "only the other host, and only with a thread")
        XCTAssertEqual(f.capcom.earlier.first?.thread.items.map(\.text), ["From before the move."])
        XCTAssertNil(f.capcom.earlier.first?.thread.session, "read-only: no session to answer in")
        await f.setActive(false)
    }

    func testPinnedProposalsLine() async throws {
        let a = await serviceHab("bench")
        await a.setCapability("logbook", true)
        let f = await fleet([a])
        await f.capcom.openStanding()
        let session = try XCTUnwrap(f.capcom.standing?.target.session)
        let rec = try JSONDecoder().decode(LogbookRecord.self, from: Data(#"{"id":"p1","title":"Call the plumber","type":"proposal","status":"pending"}"#.utf8))
        await a.setCapcomProposals(session: session, [rec, rec])
        let (m, t) = try await open(f)
        await eventually("proposals") { t.proposals?.count == 2 }
        XCTAssertEqual(CapcomThread.proposalsLine(2), "2 proposals to review")
        XCTAssertEqual(CapcomThread.proposalsLine(1), "1 proposal to review")
        XCTAssertNil(CapcomThread.proposalsLine(0))
        m.stop()
        await f.setActive(false)
    }

    // MARK: - The communication notification's Reply (7.6)

    func testReplyIntent() {
        let meta = PushMeta(v: 1, kind: "capcom", hab: "bench", session: "capcom-s1")
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.capcomReplyAction, meta: meta, text: " thanks "),
                       .capcomReply(host: "bench", session: "capcom-s1", text: "thanks"))
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.capcomReplyAction, meta: meta, text: "  "), .open(meta))
        var noSession = meta
        noSession.session = nil
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.capcomReplyAction, meta: noSession, text: "x"), .open(noSession))
    }

    func testReplySendsOrWaitsInTheComposer() async throws {
        let a = await serviceHab("bench")
        let f = await fleet([a])
        await f.capcom.openStanding()
        let session = try XCTUnwrap(f.capcom.standing?.target.session)
        let answerer = ApprovalAnswerer(live: { host in f.connection(host) },
                                        oneShot: { _ in throw TransportError("no route") }, clientLabel: "test")
        var posted: [String] = []
        let ok = await CapcomReply.send(host: "bench", session: session, text: "sounds good", answerer: answerer, center: f.capcom) { t, _ in posted.append(t) }
        XCTAssertTrue(ok)
        let sends = await a.sends
        XCTAssertEqual(sends.last?.session, session)
        XCTAssertEqual(sends.last?.text, "sounds good")
        XCTAssertNotNil(sends.last?.idempotencyKey, "a retried reply lands once")
        XCTAssertTrue(posted.isEmpty)
        // The hab refuses: "Not sent", and the text waits in the composer.
        await a.failNextSend(kind: .unavailable, hint: "the thread is not writable")
        let failed = await CapcomReply.send(host: "bench", session: session, text: "see you at 8", answerer: answerer, center: f.capcom) { t, _ in posted.append(t) }
        XCTAssertFalse(failed)
        XCTAssertEqual(posted, ["Not sent"])
        XCTAssertEqual(f.capcom.unsent, "see you at 8")
        let m = try XCTUnwrap(f.capcom.standing)
        f.capcom.takeUnsent(into: m)
        XCTAssertEqual(m.handoff, "see you at 8")
        XCTAssertNil(f.capcom.unsent)
        await f.setActive(false)
    }
}
