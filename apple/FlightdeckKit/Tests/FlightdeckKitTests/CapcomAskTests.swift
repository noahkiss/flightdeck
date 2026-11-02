import FlightdeckShared
import Foundation
import HabAPI
import UserNotifications
import XCTest
@testable import FlightdeckKit

/// "Ask an agent" from a notification (capcom.md 8.2, 8.5; app.md 8.8): the Ask categories
/// and the `ask` feature, the action that opens Capcom on the hab named by `capcom_home`
/// with `capcom_context` attached unread, "New conversation", and the failure messages.
@MainActor
final class CapcomAskTests: XCTestCase {
    private let homeDevice = "SHA256:fake-home"
    private let senderDevice = "SHA256:fake-sender"

    /// Two habs: "s" sends banners, "c" runs Capcom and knows "s" as the peer `sender`.
    private func rig(activate: Bool = true) async -> (Fleet, sender: FakeHab, home: FakeHab) {
        let sender = await sampleHab("sender")
        let home = FakeHab(hostID: "home", capabilities: ["agent-state", "transcripts", "capcom", "attachments"])
        await home.addCapcomPeer(label: "sender-lan", deviceID: senderDevice)
        await home.addNotice(id: "n7", title: "Build failed")
        await home.addCapcomCommand(pane: "%2", id: 4, line: "make test")
        let fleet = Fleet()
        fleet.add(HabConnection(id: "s", transport: sender.transport(), timing: fastTiming()), label: "laptop")
        fleet.add(HabConnection(id: "c", transport: home.transport(), timing: fastTiming()), label: "desk")
        if activate {
            await fleet.setActive(true)
            await eventually("online") { fleet.board.habs.allSatisfy { $0.state.status == .online } }
        }
        return (fleet, sender, home)
    }

    private func meta(_ ref: String = "k1", host: String? = "SHA256:fake-sender", home: String? = "SHA256:fake-home", kind: String = "permission") -> PushMeta {
        PushMeta(kind: kind, hab: "sender", session: "s1", pane: "%1", request: "r1",
                 capcomContext: .init(kind: "notification", ref: ref, host: host), capcomHome: home)
    }

    // MARK: - Categories and registration

    func testAskCategoriesAddAskLastAndOpenTheApp() {
        let cats = Dictionary(uniqueKeysWithValues: HabActions.categories().map { ($0.identifier, $0) })
        XCTAssertEqual(cats[PushCategory.approvalAsk]?.actions.map(\.identifier), [PushCategory.denyAction, PushCategory.approveAction, PushCategory.askAction])
        XCTAssertEqual(cats[PushCategory.approvalDenyAsk]?.actions.map(\.identifier), [PushCategory.denyAction, PushCategory.openAction, PushCategory.askAction])
        XCTAssertEqual(cats[PushCategory.openAsk]?.actions.map(\.identifier), [PushCategory.openAction, PushCategory.askAction])
        XCTAssertEqual(cats[PushCategory.ask]?.actions.map(\.identifier), [PushCategory.askAction])
        let ask = cats[PushCategory.ask]!.actions[0]
        XCTAssertEqual(ask.title, "Ask an agent")
        XCTAssertTrue(ask.options.contains(.foreground))
        // Approve and Deny keep their unlock rule in the Ask set.
        for a in cats[PushCategory.approvalAsk]!.actions where a.identifier != PushCategory.askAction {
            XCTAssertTrue(a.options.contains(.authenticationRequired), a.identifier)
        }
        // The old sets stay for a hab that sends them.
        XCTAssertEqual(cats[PushCategory.approval]?.actions.count, 2)
    }

    /// `push.register` lists the `ask` feature in the same build that registers the categories.
    func testRegistrationSendsTheAskFeature() async {
        let hab = await blockedHab(push: true)
        let fleet = Fleet()
        let reg = PushRegistrar(defaults: emptyDefaults(), environment: .development)
        fleet.push = reg
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "desk")
        reg.setDeviceToken(Data([1]))
        await fleet.setActive(true)
        await eventually("registered") { await hab.pushRegisters.count == 1 }
        let calls = await hab.pushRegisters
        XCTAssertEqual(calls.first?.features, ["ask"])
        let ids = Set(HabActions.categories().map(\.identifier))
        XCTAssertTrue(ids.isSuperset(of: [PushCategory.approvalAsk, PushCategory.approvalDenyAsk, PushCategory.openAsk, PushCategory.ask]))
        await fleet.setActive(false)
    }

    func testAskActionMapsToAskAndPayloadDecodes() throws {
        let raw: [String: Any] = ["v": 1, "kind": "notice", "hab": "sender", "notice": "k1",
                                  "capcom_context": ["kind": "notification", "ref": "k1", "host": senderDevice],
                                  "capcom_home": homeDevice]
        let m = try XCTUnwrap(PushMeta(userInfo: [PushMeta.userInfoKey: raw]))
        XCTAssertEqual(m.capcomContext, .init(kind: "notification", ref: "k1", host: senderDevice))
        XCTAssertEqual(m.capcomHome, homeDevice)
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.askAction, meta: m), .ask(m))
        // A permission banner's Ask never becomes an answer.
        let p = meta()
        XCTAssertEqual(NotificationAnswer.intent(action: PushCategory.askAction, meta: p), .ask(p))
        XCTAssertEqual(PushMeta(userInfo: [PushMeta.userInfoKey: p.userInfoValue]), p, "round-trips")
    }

    // MARK: - The action

    /// Ask opens Capcom on the hab whose device id is `capcom_home`, with the item unread.
    func testAskOpensCapcomOnTheHomeHabWithTheContextUnread() async throws {
        let (fleet, sender, home) = await rig()
        await fleet.capcom.ask(meta())
        await eventually("opened") { fleet.capcom.openRequest != nil }
        XCTAssertNil(fleet.capcom.failure)
        XCTAssertEqual(fleet.capcom.openRequest?.habID, "c")
        let opens = await home.capcomOpens
        XCTAssertEqual(opens.map(\.context), [[CapcomContextRef(host: senderDevice, kind: .notification, ref: "k1")]])
        XCTAssertNil(opens.first?.new)
        let other = await sender.capcomOpens
        XCTAssertTrue(other.isEmpty)
        let answers = await sender.answers
        XCTAssertTrue(answers.isEmpty, "Ask never answers the request")
        let m = try XCTUnwrap(fleet.capcom.held)
        XCTAssertEqual(m.target.session, "capcom-s1")
        XCTAssertEqual(m.pending.map(\.host), ["sender-lan"])
        await eventually("capcom") { m.context.isCapcom }
        XCTAssertEqual(CapcomChip.label(m.pending[0], hostID: m.context.hostID), "Notice k1 · sender-lan")
        // Send with no text delivers the event alone.
        m.send("")
        await eventually("delivered") { await home.capcomDelivered.count == 1 }
        await eventually("cleared") { m.pending.isEmpty }
        await fleet.setActive(false)
    }

    /// Launched from the banner, the app finds the hab by the device id the hab book kept,
    /// and asks once it comes online.
    func testAskWaitsForTheHomeHabToComeOnline() async throws {
        let (fleet, _, home) = await rig(activate: false)
        fleet.book = HabBook(url: nil)
        fleet.book?.record(host: "home", connection: "c", label: "desk", device: homeDevice)
        await fleet.capcom.ask(meta())
        XCTAssertNil(fleet.capcom.openRequest)
        await fleet.setActive(true)
        await eventually("opened after online") { fleet.capcom.openRequest?.habID == "c" }
        let opens = await home.capcomOpens
        XCTAssertEqual(opens.count, 1)
        XCTAssertNil(fleet.capcom.failure)
        await fleet.setActive(false)
    }

    func testAskFailures() async throws {
        let (fleet, _, home) = await rig(activate: false)
        fleet.capcom.askWait = .milliseconds(50)
        // No hab with that device id: with welcomes still to come it waits, then says so.
        await fleet.capcom.ask(meta(home: "SHA256:someone-else"))
        XCTAssertNil(fleet.capcom.failure)
        await eventually("not found") { fleet.capcom.failure != nil }
        XCTAssertEqual(fleet.capcom.failure?.message, "The hab that runs Capcom is not in this app.")
        // No context in the payload.
        fleet.capcom.failure = nil
        var bare = meta()
        bare.capcomContext = nil
        await fleet.capcom.ask(bare)
        XCTAssertEqual(fleet.capcom.failure?.message, "This notification names no Capcom host.")
        // The hab never comes online.
        fleet.capcom.failure = nil
        fleet.book = HabBook(url: nil)
        fleet.book?.record(host: "home", connection: "c", label: "desk", device: homeDevice)
        await fleet.capcom.ask(meta())
        await eventually("unreachable") { fleet.capcom.failure != nil }
        XCTAssertEqual(fleet.capcom.failure?.message, "Could not reach desk to ask an agent.")
        let none = await home.capcomOpens
        XCTAssertTrue(none.isEmpty)
        // A refusal shows the hab's text.
        fleet.capcom.failure = nil
        await fleet.setActive(true)
        await eventually("online") { fleet.board.habs.allSatisfy { $0.state.status == .online } }
        await home.failNextCapcom(kind: .unavailable, hint: "Capcom runs on laptop; open it there")
        await fleet.capcom.ask(meta())
        XCTAssertEqual(fleet.capcom.failure?.message, "Capcom runs on laptop; open it there")
        XCTAssertNil(fleet.capcom.openRequest)
        // Every hab online and none with the device id: at once.
        fleet.capcom.failure = nil
        await fleet.capcom.ask(meta(home: "SHA256:someone-else"))
        XCTAssertEqual(fleet.capcom.failure?.message, "The hab that runs Capcom is not in this app.")
        await fleet.setActive(false)
    }

    /// The first launch after this update: the hab book holds no device ids yet. The Ask waits
    /// for the welcomes and finds the hab by the one that names it.
    func testAskFindsTheHabOnceItsWelcomeArrives() async throws {
        let (fleet, _, home) = await rig(activate: false)
        fleet.book = HabBook(url: nil)
        fleet.book?.record(host: "home", connection: "c", label: "desk")
        await fleet.capcom.ask(meta())
        XCTAssertNil(fleet.capcom.failure)
        await fleet.setActive(true)
        await eventually("opened") { fleet.capcom.openRequest?.habID == "c" }
        let opens = await home.capcomOpens
        XCTAssertEqual(opens.count, 1)
        XCTAssertNil(fleet.capcom.failure)
        await fleet.setActive(false)
    }

    /// A session the board never shows does not open later, and the model is let go.
    func testOpenRequestLapses() async throws {
        let (fleet, _, _) = await rig()
        fleet.capcom.askWait = .milliseconds(50)
        await fleet.capcom.ask(meta())
        XCTAssertNotNil(fleet.capcom.openRequest)
        XCTAssertNotNil(fleet.capcom.held)
        await eventually("lapsed") { fleet.capcom.openRequest == nil && fleet.capcom.held == nil }
        await fleet.setActive(false)
    }

    /// A file that cannot go up again for the new conversation stays on the old one.
    func testNewConversationKeepsAFileThatDidNotMove() async throws {
        let (fleet, _, home) = await rig()
        await fleet.capcom.ask(meta())
        let old = try XCTUnwrap(fleet.capcom.held)
        await eventually("capcom") { old.context.isCapcom }
        _ = await old.attachFile(DraftAttachment(name: "log.txt", type: "text/plain", data: Data("boom".utf8)))
        XCTAssertEqual(old.pending.count, 2)
        await home.failNextAttachmentWrites(10)
        let ok = await fleet.capcom.newConversation(from: old, draft: "")
        XCTAssertTrue(ok)
        let m = try XCTUnwrap(fleet.capcom.held)
        XCTAssertEqual(m.pending.map(\.kind), [.notification])
        XCTAssertEqual(old.pending.map(\.kind), [.file], "the file stays where it can still be sent")
        let detaches = await home.capcomDetaches
        XCTAssertEqual(detaches.map(\.kind), [.notification])
        await fleet.setActive(false)
    }

    // MARK: - New conversation

    func testNewConversationMovesTheChipsAndTheDraft() async throws {
        let (fleet, _, home) = await rig()
        await fleet.capcom.ask(meta())
        let old = try XCTUnwrap(fleet.capcom.held)
        await eventually("capcom") { old.context.isCapcom }
        _ = await old.attachCommand(pane: "%2", command: 4)
        let file = DraftAttachment(name: "log.txt", type: "text/plain", data: Data("boom".utf8))
        _ = await old.attachFile(file)
        XCTAssertEqual(old.pending.count, 3)
        fleet.capcom.shown()

        let ok = await fleet.capcom.newConversation(from: old, draft: "is this safe?")
        XCTAssertTrue(ok)
        let opens = await home.capcomOpens
        XCTAssertEqual(opens.last?.new, true)
        XCTAssertEqual(opens.last?.context, [CapcomContextRef(host: "sender-lan", kind: .notification, ref: "k1"),
                                             CapcomContextRef(kind: .command, ref: "%2:4")], "a file cannot ride the open")
        let m = try XCTUnwrap(fleet.capcom.held)
        XCTAssertFalse(m === old)
        XCTAssertEqual(m.target.session, "capcom-s2")
        XCTAssertEqual(fleet.capcom.openRequest?.session, "capcom-s2")
        XCTAssertEqual(m.handoff, "is this safe?")
        XCTAssertEqual(m.pending.map(\.kind), [.notification, .command, .file])
        let stored = await home.storedFiles
        let fileChip = try XCTUnwrap(m.pending.last)
        XCTAssertEqual(stored[fileChip.ref]?.put.session, "capcom-s2", "the file went up again for the new session")
        XCTAssertTrue(old.pending.isEmpty, "detached from the old conversation")
        let oldWaiting = await home.capcomPending("capcom-s1")
        XCTAssertTrue(oldWaiting.isEmpty)
        await fleet.setActive(false)
    }

    func testRefusedNewConversationKeepsEverything() async throws {
        let (fleet, _, home) = await rig()
        await fleet.capcom.ask(meta())
        let old = try XCTUnwrap(fleet.capcom.held)
        fleet.capcom.shown()
        await home.failNextCapcom(kind: .conflict, hint: "8 context items already wait")
        let ok = await fleet.capcom.newConversation(from: old, draft: "hi")
        XCTAssertFalse(ok)
        XCTAssertEqual(old.notice, "8 context items already wait")
        XCTAssertEqual(old.pending.count, 1)
        XCTAssertNil(fleet.capcom.openRequest)
        let detaches = await home.capcomDetaches
        XCTAssertTrue(detaches.isEmpty)
        await fleet.setActive(false)
    }
}
