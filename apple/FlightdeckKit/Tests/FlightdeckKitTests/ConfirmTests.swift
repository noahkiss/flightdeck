import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// Confirmation cards (capcom.md 6.1-6.4): the book a hab's snapshot and
/// events build, the center's placement, edits, answers and countdown.
final class ConfirmTests: XCTestCase {
    static let t0 = Date(timeIntervalSince1970: 1_800_000_000)

    static func start(host: String = "boxb", name: String? = "claude-3fa9c2", model: String? = nil, seed: String? = "Fix the flaky test") -> ConfirmStart {
        ConfirmStart(branch: "main", folder: "/work/tide-tables", harness: "claude", host: host, model: model, name: name, project: "tide-tables", seed: seed)
    }

    static func confirmation(_ id: String, from: String? = "claude", state: ConfirmState = .open, record: Bool? = nil,
                             created: Date = t0, ttl: TimeInterval = 3600) -> Confirmation {
        Confirmation(created: Fixture.stamp(created), expiresAt: state == .open && record != true ? Fixture.stamp(created.addingTimeInterval(ttl)) : nil,
                     from: from, id: id, kind: .start, record: record, start: start(), state: state)
    }

    static func event(_ type: String, _ c: Confirmation, seq: UInt64 = 1) throws -> Event {
        let v = try JSONDecoder().decode(JSONValue.self, from: JSONEncoder().encode(c))
        return Event(data: v, id: c.id, revision: nil, seq: seq, type: type)
    }

    static func closed(_ c: Confirmation, _ state: ConfirmState, at: Date = t0.addingTimeInterval(5)) -> Confirmation {
        var c = c
        c.state = state
        c.closed = Fixture.stamp(at)
        return c
    }

    // MARK: - The book

    func testSnapshotThenEventsAndWithdraw() throws {
        var b = ConfirmBook()
        let a = Self.confirmation("a")
        b.reset([a], enabled: true, newEpoch: true)
        XCTAssertEqual(b.open.map(\.id), ["a"])

        let r = Self.confirmation("b")
        XCTAssertTrue(b.apply(try Self.event("confirm.requested", r)))
        XCTAssertEqual(b.open.map(\.id), ["a", "b"])
        // The same requested twice changes nothing.
        XCTAssertFalse(b.apply(try Self.event("confirm.requested", r)))

        XCTAssertTrue(b.apply(try Self.event("confirm.closed", Self.closed(a, .cancelled))))
        XCTAssertEqual(b.open.map(\.id), ["b"])
        XCTAssertEqual(b.closed.map(\.id), ["a"])
        XCTAssertEqual(b.find("a")?.state, .cancelled)
        // A late requested for a closed one never brings it back.
        XCTAssertFalse(b.apply(try Self.event("confirm.requested", a)))
        XCTAssertEqual(b.open.map(\.id), ["b"])
        // Other events change nothing.
        XCTAssertFalse(b.apply(try Self.event("session.updated", r)))
    }

    func testCapabilityGate() throws {
        var b = ConfirmBook()
        b.reset([Self.confirmation("a")], enabled: false, newEpoch: true)
        XCTAssertFalse(b.enabled)
        XCTAssertTrue(b.open.isEmpty)
        XCTAssertFalse(b.apply(try Self.event("confirm.requested", Self.confirmation("b"))))
        XCTAssertTrue(b.open.isEmpty)
    }

    func testReconnectKeepsClosedWithinAnEpoch() throws {
        var b = ConfirmBook()
        b.reset([Self.confirmation("a"), Self.confirmation("b")], enabled: true, newEpoch: true)
        b.apply(try Self.event("confirm.closed", Self.closed(Self.confirmation("a"), .done)))
        // Same epoch: the snapshot replaces the open ones; closed ones stay.
        b.reset([Self.confirmation("c")], enabled: true, newEpoch: false)
        XCTAssertEqual(b.open.map(\.id), ["c"])
        XCTAssertEqual(b.closed.map(\.id), ["a"])
        // A new epoch drops them.
        b.reset(nil, enabled: true, newEpoch: true)
        XCTAssertTrue(b.open.isEmpty)
        XCTAssertTrue(b.closed.isEmpty)
    }

    func testClosedListIsCapped() throws {
        var b = ConfirmBook()
        b.reset(nil, enabled: true, newEpoch: true)
        for i in 0..<(ConfirmBook.closedLimit + 5) {
            b.apply(try Self.event("confirm.closed", Self.closed(Self.confirmation("c\(i)"), .done)))
        }
        XCTAssertEqual(b.closed.count, ConfirmBook.closedLimit)
        XCTAssertEqual(b.closed.first?.id, "c\(ConfirmBook.closedLimit + 4)")
    }

    // MARK: - The center over hand-made states

    func habState(_ book: ConfirmBook, online: Bool = true, offset: TimeInterval? = nil) -> HabState {
        var s = HabState()
        s.status = online ? .online : .offline
        s.welcome = Welcome(build: "test", capabilities: ["confirm"], clientId: "c", epoch: "e", host: HostInfo(id: "boxa"),
                            reconnectToken: "t", revision: 1, seq: 0)
        s.store.reset(StructureResult(host: HostInfo(id: "boxa"), panes: [], projects: [], seq: 0, sessions: [
            Fixture.session("s1", name: "claude", pane: "%1", project: nil),
            Fixture.session("s9", name: "capcom", pane: "%9", project: nil),
        ]))
        s.confirmations = book
        s.clockOffset = offset
        return s
    }

    @MainActor
    func center(_ book: ConfirmBook, online: Bool = true, offset: TimeInterval? = nil, now: Date = t0,
                answer: @escaping ConfirmCenter.Answer = { _, _ in throw TransportError("unused") }) -> (ConfirmCenter, Board) {
        let board = Board()
        board.register(id: "a", label: "boxa", endpoint: "fake")
        board.update("a", habState(book, online: online, offset: offset))
        let n = now
        return (ConfirmCenter(board: board, now: { n }, answer: answer), board)
    }

    func book(_ cs: [Confirmation], closed: [Confirmation] = []) throws -> ConfirmBook {
        var b = ConfirmBook()
        b.reset(cs, enabled: true, newEpoch: true)
        for c in closed { b.apply(try Self.event("confirm.closed", c)) }
        return b
    }

    @MainActor
    func testPlacement() throws {
        let b = try book([
            Self.confirmation("agent"),
            Self.confirmation("capcom", from: "capcom"),
            Self.confirmation("outside", from: nil),
            Self.confirmation("rec", state: .running, record: true),
        ], closed: [Self.closed(Self.confirmation("old"), .done)])
        let (c, _) = center(b)
        c.isCapcom = { _, name in name == "capcom" }
        let s1 = SessionTarget(habID: "a", session: "s1", pane: "%1")
        // The agent's session view: its open ones, records included, then closed.
        XCTAssertEqual(c.cards(for: s1).map(\.confirmation.id), ["agent", "rec", "old"])
        // The approvals area: questions only, not Capcom's, and the one from outside a pane.
        XCTAssertEqual(c.approvals.map(\.confirmation.id), ["agent", "outside"])
        XCTAssertEqual(c.capcom.map(\.confirmation.id), ["capcom"])
        XCTAssertEqual(c.waitingCount, 3)
        // Without a marker, Capcom's card is an ordinary agent's card.
        c.isCapcom = { _, _ in false }
        XCTAssertEqual(c.approvals.map(\.confirmation.id), ["agent", "capcom", "outside"])
        XCTAssertEqual(c.cards(for: SessionTarget(habID: "a", session: "s9", pane: "%9")).map(\.confirmation.id), ["capcom"])
    }

    /// A session the hab files with kind capcom puts its cards in the Capcom view.
    @MainActor
    func testCapcomKindRoutesToCapcom() throws {
        let b = try book([Self.confirmation("capcom", from: "capcom"), Self.confirmation("agent")])
        let (c, board) = center(b)
        var st = habState(b)
        var cap = Fixture.session("s9", name: "capcom", pane: "%9", project: nil)
        cap.kind = .capcom
        st.store.reset(StructureResult(host: HostInfo(id: "boxa"), panes: [], projects: [], seq: 0, sessions: [
            Fixture.session("s1", name: "claude", pane: "%1", project: nil), cap,
        ]))
        board.update("a", st)
        XCTAssertEqual(c.capcom.map(\.confirmation.id), ["capcom"])
        XCTAssertEqual(c.approvals.map(\.confirmation.id), ["agent"])
    }

    @MainActor
    func testRecordHasNoButtons() throws {
        let b = try book([Self.confirmation("rec", state: .running, record: true)])
        let (c, _) = center(b)
        let card = try XCTUnwrap(c.cards.first)
        XCTAssertEqual(c.phase(card), .record)
        XCTAssertFalse(c.hasButtons(card))
        XCTAssertFalse(c.canAnswer(card))
        XCTAssertNil(c.remaining(card))
        XCTAssertTrue(c.approvals.isEmpty)
    }

    @MainActor
    func testCountdownAndExpiry() throws {
        let b = try book([Self.confirmation("a", ttl: 252)])
        var (c, _) = center(b, now: Self.t0)
        var card = try XCTUnwrap(c.cards.first)
        XCTAssertEqual(c.remaining(card), 252)
        XCTAssertEqual(ConfirmCenter.countdown(252), "4m 12s")
        XCTAssertEqual(ConfirmCenter.countdown(3720), "1h 02m")
        XCTAssertEqual(ConfirmCenter.countdown(9.2), "10s")
        XCTAssertTrue(c.canAnswer(card))

        // The hab's clock runs 60 s ahead: the device sees 60 s less.
        (c, _) = center(b, offset: 60, now: Self.t0)
        card = try XCTUnwrap(c.cards.first)
        XCTAssertEqual(c.remaining(card), 192)

        // Past expires_at: buttons show, but off.
        (c, _) = center(b, now: Self.t0.addingTimeInterval(253))
        card = try XCTUnwrap(c.cards.first)
        XCTAssertEqual(c.remaining(card), 0)
        XCTAssertEqual(c.phase(card), .expired)
        XCTAssertTrue(c.hasButtons(card))
        XCTAssertFalse(c.canAnswer(card))
    }

    @MainActor
    func testOfflineHabTurnsButtonsOff() throws {
        let (c, _) = center(try book([Self.confirmation("a")]), online: false)
        let card = try XCTUnwrap(c.cards.first)
        XCTAssertEqual(c.phase(card), .offline)
        XCTAssertFalse(c.canAnswer(card))
    }

    @MainActor
    func testConflictShowsAlreadyAnswered() async throws {
        let (c, _) = center(try book([Self.confirmation("a")])) { _, p in
            throw RPCError(code: -32000, message: "conflict", data: ErrorData(hint: "confirmation \(p.id) was already answered", kind: .conflict, seq: 1))
        }
        let card = try XCTUnwrap(c.cards.first)
        await c.confirm(card)
        XCTAssertEqual(c.phase(card), .alreadyAnswered)
        XCTAssertFalse(c.canAnswer(card))
    }

    @MainActor
    func testOtherRefusalKeepsButtonsOn() async throws {
        let (c, _) = center(try book([Self.confirmation("a")])) { _, _ in
            throw RPCError(code: -32000, message: "invalid", data: ErrorData(hint: "folder is not absolute", kind: .invalid, seq: 1))
        }
        let card = try XCTUnwrap(c.cards.first)
        await c.cancel(card)
        XCTAssertEqual(c.phase(card), .failed("folder is not absolute"))
        XCTAssertTrue(c.canAnswer(card))
    }

    @MainActor
    func testEditsGoWithTheAnswer() async throws {
        var sent: [ConfirmAnswerParams] = []
        let a = Self.confirmation("a")
        let (c, _) = center(try book([a])) { _, p in
            sent.append(p)
            return ConfirmationResult(confirmation: a, seq: 1)
        }
        let card = try XCTUnwrap(c.cards.first)
        // No change: a plain confirm.
        await c.confirm(card)
        XCTAssertEqual(sent.last, ConfirmAnswerParams(choice: .confirm, id: "a"))

        // A new host with the same folder: both go.
        var d = try XCTUnwrap(c.draft(card))
        d.host = "boxc"
        d.harness = "codex"
        d.model = "gpt-5"
        d.seed = "New seed"
        c.setDraft(d, for: card)
        XCTAssertTrue(c.isEdited(card))
        await c.confirm(card)
        XCTAssertEqual(sent.last?.edits, ConfirmEdits(folder: "/work/tide-tables", harness: "codex", host: "boxc", model: "gpt-5", seed: "New seed"))
        XCTAssertFalse(c.isEdited(card), "a sent answer drops the draft")

        // An emptied field keeps the card's value; a new host needs its folder.
        d = try XCTUnwrap(c.draft(card))
        d.model = ""
        d.host = "boxc"
        d.folder = ""
        c.setDraft(d, for: card)
        let before = sent.count
        await c.confirm(card)
        XCTAssertEqual(sent.count, before, "nothing is sent while the draft has a problem")
        XCTAssertEqual(c.phase(card), .failed("Give the folder on the new host"))
        XCTAssertEqual(ConfirmEdits(), ConfirmDraft(Self.start()).edits(against: Self.start()) ?? ConfirmEdits())
    }

    func testDraftRules() {
        let s = Self.start()
        var d = ConfirmDraft(s)
        XCTAssertNil(d.edits(against: s))
        d.folder = "/work/other"
        XCTAssertEqual(d.edits(against: s), ConfirmEdits(folder: "/work/other"))
        d = ConfirmDraft(s)
        d.seed = String(repeating: "x", count: ConfirmDraft.seedLimit + 1)
        XCTAssertNotNil(d.problem(against: s))
        d.seed = ""
        XCTAssertNil(d.edits(against: s), "an emptied seed keeps the old one")
    }

    func testOutcomeText() {
        var c = Self.closed(Self.confirmation("a"), .done)
        c.result = ConfirmResult(host: "boxb", name: "claude-3fa9c2", pane: "%4", session: "s4")
        XCTAssertEqual(ConfirmCenter.outcome(c), "Started claude-3fa9c2 on boxb")
        c.state = .expired
        XCTAssertEqual(ConfirmCenter.outcome(c), "Expired: nothing ran")
    }

    // MARK: - End to end over the fake hab

    @MainActor
    func testOverTheConnection() async throws {
        let hab = await sampleHab("boxa")
        await hab.setCapability("confirm", true)
        await hab.requestConfirmation(Self.confirmation("snap", created: Date()))
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        let board = Board()
        board.register(id: "a", label: nil, endpoint: "fake")
        let feed = Task { @MainActor in for await s in conn.updates { board.update("a", s) } }
        defer { feed.cancel() }
        let center = ConfirmCenter(board: board, answer: { _, p in try await conn.call(HabMethods.confirmAnswer, p) })
        await conn.start()
        await eventually("snapshot card") { center.cards.map(\.confirmation.id) == ["snap"] }
        XCTAssertEqual(center.cards.first?.session?.session, "s1")

        await hab.requestConfirmation(Self.confirmation("live", created: Date()))
        await eventually("event card") { center.cards.count == 2 }

        // Confirm with an edit: the hab runs it and the card withdraws.
        let card = try XCTUnwrap(center.cards.first { $0.confirmation.id == "live" })
        var d = try XCTUnwrap(center.draft(card))
        d.model = "opus"
        center.setDraft(d, for: card)
        await center.confirm(card)
        let answers = await hab.confirmAnswers
        XCTAssertEqual(answers.last?.edits, ConfirmEdits(model: "opus"))
        await eventually("withdrawn") { center.approvals.map(\.confirmation.id) == ["snap"] }
        XCTAssertEqual(center.cards.first { $0.confirmation.id == "live" }?.confirmation.state, .done)

        let done = try XCTUnwrap(center.cards.first { $0.confirmation.id == "live" })
        XCTAssertEqual(center.phase(done), .closed)
        XCTAssertFalse(center.hasButtons(done))
        // Answered on another client while this card is still on screen: conflict.
        let snapCard = try XCTUnwrap(center.cards.first { $0.confirmation.id == "snap" })
        await hab.closeConfirmation("snap", state: .cancelled, answeredBy: "c99")
        await center.confirm(snapCard) // the stale card still on screen
        XCTAssertEqual(center.phase(snapCard), .alreadyAnswered)
        await conn.stop()
    }

    @MainActor
    func testNoCapabilityNoCards() async throws {
        let hab = await sampleHab("boxa")
        await hab.requestConfirmation(Self.confirmation("x", created: Date()))
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("online") { await conn.state.status == .online }
        await hab.requestConfirmation(Self.confirmation("y", created: Date()))
        await hab.put(Fixture.session("s3", name: "late", pane: nil, project: nil))
        await eventually("later event applied") { await conn.currentStore().sessions["s3"] != nil }
        let book = await conn.state.confirmations
        XCTAssertFalse(book.enabled)
        XCTAssertTrue(book.open.isEmpty)
        await conn.stop()
    }

    // MARK: - Fix round

    /// A confirmed start that failed stays in the approvals area with its outcome until dismissed.
    @MainActor
    func testFailedStartStaysUntilDismissed() async throws {
        let hab = await sampleHab("boxa")
        await hab.setCapability("confirm", true)
        await hab.requestConfirmation(Self.confirmation("f", created: Date()))
        await hab.setConfirmOutcome(.failed, reason: "cwd is not a directory")
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        let board = Board()
        board.register(id: "a", label: nil, endpoint: "fake")
        let center = ConfirmCenter(board: board, answer: { _, p in try await conn.call(HabMethods.confirmAnswer, p) })
        let feed = Task { @MainActor in for await s in conn.updates { board.update("a", s); center.boardChanged() } }
        defer { feed.cancel() }
        await conn.start()
        await eventually("card") { center.approvals.count == 1 }
        let card = try XCTUnwrap(center.approvals.first)
        await center.confirm(card)
        await eventually("closed") { center.cards.first?.confirmation.isClosed == true }
        let kept = try XCTUnwrap(center.approvals.first)
        XCTAssertEqual(kept.confirmation.state, .failed)
        XCTAssertTrue(center.isHeld(kept))
        XCTAssertEqual(ConfirmCenter.outcome(kept.confirmation), "Failed: cwd is not a directory")
        center.boardChanged()
        XCTAssertEqual(center.approvals.count, 1, "pruning keeps a held card")
        center.dismiss(kept)
        XCTAssertTrue(center.approvals.isEmpty)
        await conn.stop()
    }

    /// A done answer leaves nothing in the approvals area.
    @MainActor
    func testDoneStartIsNotHeld() async throws {
        let a = Self.confirmation("a")
        var done = Self.closed(a, .done)
        done.result = ConfirmResult(host: "boxb", pane: "%2", session: "s2")
        let (c, _) = center(try book([a])) { _, _ in ConfirmationResult(confirmation: done, seq: 1) }
        await c.confirm(try XCTUnwrap(c.cards.first))
        XCTAssertFalse(c.isHeld(try XCTUnwrap(c.cards.first)))
    }

    /// A second tap while the first answer is on its way sends nothing.
    @MainActor
    func testSecondTapWhileSending() async throws {
        var calls = 0
        var release: CheckedContinuation<Void, Never>?
        let a = Self.confirmation("a")
        let (c, _) = center(try book([a])) { _, _ in
            calls += 1
            await withCheckedContinuation { release = $0 }
            return ConfirmationResult(confirmation: a, seq: 1)
        }
        let card = try XCTUnwrap(c.cards.first)
        let first = Task { await c.confirm(card) }
        await eventually("sending") { c.phase(card) == .sending && release != nil }
        XCTAssertFalse(c.canAnswer(card))
        await c.confirm(card)
        await c.cancel(card)
        XCTAssertEqual(calls, 1)
        release?.resume()
        await first.value
        XCTAssertEqual(calls, 1)
    }

    /// Through HabConnection: closed cards stay across a reconnect in one epoch, a card
    /// that closed while offline learns its outcome from confirm.list, a running record
    /// comes back, and a new epoch drops them all.
    @MainActor
    func testReconnectAndNewEpoch() async throws {
        let hab = await sampleHab("boxa")
        await hab.setCapability("confirm", true)
        await hab.requestConfirmation(Self.confirmation("a", created: Date()))
        await hab.requestConfirmation(Self.confirmation("b", created: Date()))
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("two open") { await conn.state.confirmations.open.count == 2 }
        await hab.closeConfirmation("a", state: .cancelled)
        await eventually("a closed") { await conn.state.confirmations.closed.map(\.id) == ["a"] }

        await conn.stop()
        await hab.closeConfirmation("b", state: .expired, reason: "no answer within 60 minutes; nothing ran")
        var rec = Self.confirmation("r", state: .running, record: true, created: Date())
        rec.expiresAt = nil
        await hab.requestConfirmation(rec)
        await conn.start()
        await eventually("outcome recovered") { await conn.state.confirmations.find("b")?.state == .expired }
        var book = await conn.state.confirmations
        XCTAssertEqual(Set(book.closed.map(\.id)), ["a", "b"])
        XCTAssertEqual(book.open.map(\.id), ["r"], "the running record is not in the snapshot; the list brings it")

        let lists = await hab.confirmLists
        await hab.restart()
        await eventually("new epoch") { await conn.state.epochChanges == 1 }
        await eventually("list merged") { await hab.confirmLists > lists }
        book = await conn.state.confirmations
        XCTAssertTrue(book.closed.isEmpty, "a new epoch drops the closed cards")
        await conn.stop()
    }

    /// An event that arrives before the snapshot's answer waits and applies after it.
    @MainActor
    func testEventBufferedBeforeSnapshot() async throws {
        let hab = await sampleHab("boxa")
        await hab.setCapability("confirm", true)
        let early = Self.confirmation("early", created: Date())
        await hab.setSubscribeHook { await hab.requestConfirmation(early) }
        let conn = HabConnection(id: "a", transport: hab.transport(), timing: fastTiming())
        await conn.start()
        await eventually("early card") { await conn.state.confirmations.open.map(\.id) == ["early"] }
        await conn.stop()
    }

    /// Two habs with the same session name and confirmation id keep their cards,
    /// drafts and conflicts apart.
    @MainActor
    func testTwoHabsSameIDs() async throws {
        let b = try book([Self.confirmation("x")])
        let board = Board()
        board.register(id: "a", label: "boxa", endpoint: "fake")
        board.register(id: "b", label: "boxb", endpoint: "fake")
        board.update("a", habState(b))
        board.update("b", habState(b))
        let c = ConfirmCenter(board: board, now: { Self.t0 }) { habID, p in
            if habID == "a" {
                throw RPCError(code: -32000, message: "conflict", data: ErrorData(hint: "taken", kind: .conflict, seq: 1))
            }
            return ConfirmationResult(confirmation: Self.confirmation(p.id), seq: 1)
        }
        let cards = c.cards
        XCTAssertEqual(Set(cards.map(\.id)), ["a/x", "b/x"])
        let ca = try XCTUnwrap(cards.first { $0.habID == "a" })
        let cb = try XCTUnwrap(cards.first { $0.habID == "b" })
        XCTAssertEqual(c.cards(for: SessionTarget(habID: "b", session: "s1", pane: "%1")).map(\.id), ["b/x"])
        var d = try XCTUnwrap(c.draft(ca))
        d.model = "opus"
        c.setDraft(d, for: ca)
        XCTAssertTrue(c.isEdited(ca))
        XCTAssertFalse(c.isEdited(cb))
        await c.confirm(ca)
        XCTAssertEqual(c.phase(ca), .alreadyAnswered)
        XCTAssertEqual(c.phase(cb), .open)
    }

    /// Waiting questions count in the needs-you count; records and offline habs do not.
    @MainActor
    func testWaitingCountsInNeedsYou() throws {
        let board = Board()
        board.register(id: "a", label: "boxa", endpoint: "fake")
        board.register(id: "b", label: "boxb", endpoint: "fake")
        board.update("a", habState(try book([Self.confirmation("q1"), Self.confirmation("q2"), Self.confirmation("r", state: .running, record: true)])))
        board.update("b", habState(try book([Self.confirmation("q3")]), online: false))
        XCTAssertEqual(board.waitingConfirmations, 2)
        XCTAssertEqual(board.needsYouCount, 2)
    }

    /// A card's edits and notes go once it closes.
    @MainActor
    func testStateIsPrunedOnClose() async throws {
        let a = Self.confirmation("a")
        let (c, board) = center(try book([a])) { _, _ in
            throw RPCError(code: -32000, message: "invalid", data: ErrorData(hint: "bad model", kind: .invalid, seq: 1))
        }
        let card = try XCTUnwrap(c.cards.first)
        var d = try XCTUnwrap(c.draft(card))
        d.model = "opus"
        c.setDraft(d, for: card)
        await c.confirm(card)
        XCTAssertEqual(c.phase(card), .failed("bad model"))
        c.boardChanged()
        XCTAssertTrue(c.isEdited(card), "an open card keeps its draft")
        board.update("a", habState(try book([], closed: [Self.closed(a, .cancelled)])))
        c.boardChanged()
        XCTAssertFalse(c.isEdited(card))
        XCTAssertEqual(c.phase(card), .open, "the stale card value has no note left")
    }

    func testSeedCountsScalars() {
        let s = Self.start()
        var d = ConfirmDraft(s)
        // One character, two scalars: the hab counts runes, not characters.
        d.seed = String(repeating: "e\u{301}", count: 4001)
        XCTAssertEqual(d.seed.count, 4001)
        XCTAssertNotNil(d.problem(against: s))
    }

    @MainActor
    func testOrderByParsedTime() throws {
        var a = Self.confirmation("a")
        a.created = "2026-11-02T00:00:04Z"
        var b = Self.confirmation("b")
        b.created = "2026-11-02T00:00:04.5Z"
        let (c, _) = center(try book([b, a]))
        XCTAssertEqual(c.cards.map(\.confirmation.id), ["a", "b"])
    }
}
