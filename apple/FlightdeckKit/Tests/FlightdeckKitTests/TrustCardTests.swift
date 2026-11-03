import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The folder trust card's model (app.md 7.10): the prompt from the agent state, the call, and
/// its states: sending, failed with a retry, and the notice after "Don't trust".
@MainActor
final class TrustCardTests: XCTestCase {
    /// A fleet with one fake hab whose session s1 (pane %1) runs claude, waiting on its trust prompt.
    private func setup(trust: Bool = true) async -> (Fleet, FakeHab, ChatModel) {
        let hab = await sampleHab()
        await hab.setCapability(TrustCard.capability, trust)
        var a = Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .trust)
        a.source = .screen
        a.trust = TrustPrompt(folder: "/work/new-app")
        await hab.put(a)
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await fleet.setActive(true)
        await eventually("online") { fleet.board.habs.first?.state.status == .online }
        let model = fleet.chat(for: SessionTarget(habID: "h", session: "s1", pane: "%1"))
        await eventually("the prompt") { model.trustPrompt?.folder == "/work/new-app" }
        return (fleet, hab, model)
    }

    func testTrustSendsTheChoiceAndThePromptGoes() async {
        let (fleet, hab, model) = await setup()
        XCTAssertTrue(model.canAnswerTrust)
        XCTAssertEqual(model.gate, .blocked)
        await model.answerTrust(.trust)
        let sent = await hab.trustAnswers
        XCTAssertEqual(sent.map(\.choice), [.trust])
        XCTAssertEqual(sent.first?.pane, "%1")
        XCTAssertEqual(model.trustPhase, .ready)
        await eventually("prompt gone") { model.trustPrompt == nil }
        XCTAssertNil(model.notice)
        await fleet.setActive(false)
    }

    func testFailureKeepsTheChoiceAndRetrySendsItAgain() async {
        let (fleet, hab, model) = await setup()
        await hab.failNextTrust(.conflict, "pane %1: the pane no longer shows the trust dialog; answer it in the terminal")
        await model.answerTrust(.exit)
        XCTAssertEqual(model.trustPhase, .failed(.exit, "pane %1: the pane no longer shows the trust dialog; answer it in the terminal"))
        await hab.setTrustDropped(1)
        await model.retryTrust()
        let sent = await hab.trustAnswers
        XCTAssertEqual(sent.map(\.choice), [.exit, .exit])
        XCTAssertEqual(model.trustPhase, .ready)
        XCTAssertEqual(model.notice, "Not trusted. The agent exited. Start message dropped.")
        await fleet.setActive(false)
    }

    func testFailedPhaseClearsWhenThePromptGoesElsewhere() async {
        let (fleet, hab, model) = await setup()
        await hab.failNextTrust(.unavailable, "the keys went in, but the trust dialog still shows")
        await model.answerTrust(.trust)
        guard case .failed = model.trustPhase else { return XCTFail("phase \(model.trustPhase)") }
        // Answered in the terminal: the agent goes on.
        await hab.put(Fixture.agent(pane: "%1", harness: "claude", state: .idle, revision: 2))
        await eventually("ready again") { model.trustPhase == .ready && model.trustPrompt == nil }
        await fleet.setActive(false)
    }

    func testHabWithoutTheCapabilityOffersTheTerminal() async {
        let (fleet, hab, model) = await setup(trust: false)
        XCTAssertFalse(model.canAnswerTrust)
        await model.answerTrust(.trust)
        let sent = await hab.trustAnswers
        XCTAssertTrue(sent.isEmpty, "no call to a hab without trust.answer")
        XCTAssertEqual(model.trustPhase, .ready)
        await fleet.setActive(false)
    }

    func testCardWords() {
        XCTAssertEqual(TrustCard.allows("claude"), "Claude Code can read, edit and run files here.")
        XCTAssertEqual(TrustCard.allows("codex"), "Codex can read, edit and run files here.")
        XCTAssertEqual(TrustCard.declined(dropped: 0), "Not trusted. The agent exited.")
        let session = Session(id: "s1", name: "demo", revision: 1, state: .running)
        var a = Fixture.agent(pane: "%1", harness: "claude", state: .blocked, kind: .trust)
        a.trust = TrustPrompt(folder: "/w")
        XCTAssertEqual(BoardRows.title(session: session, agent: a), "Trust this folder?")
    }
}
