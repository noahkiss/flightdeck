import Foundation
import HabAPI
import XCTest
@testable import FlightdeckKit

/// The app's friction note (diagnostics.md 9.1): the menu item shows only on a hab that takes
/// notes, and Send stores the text, the session, the pane and the blocker flag on the session's
/// own hab, which answers with the trace.
@MainActor
final class FrictionTests: XCTestCase {
    private func setup(friction: Bool = true) async -> (Fleet, FakeHab, ChatModel) {
        let hab = await sampleHab()
        await hab.setCapability(FrictionNote.capability, friction)
        let fleet = Fleet()
        fleet.add(HabConnection(id: "h", transport: hab.transport(), timing: fastTiming()), label: "bench")
        await fleet.setActive(true)
        await eventually("online") { fleet.board.habs.first?.state.status == .online }
        let model = fleet.chat(for: SessionTarget(habID: "h", session: "s1", pane: "%1"))
        await eventually("the context") { model.context.online }
        return (fleet, hab, model)
    }

    func testSendStoresTheNoteOnTheSessionsHab() async {
        let (fleet, hab, model) = await setup()
        XCTAssertTrue(model.context.takesFriction)
        let note = FrictionNote(session: model.target.session, pane: model.context.pane, connection: model.connection)
        XCTAssertFalse(note.canSend, "no text yet")
        note.text = "  The diff view could not show the whole file.\n"
        note.blocker = true
        XCTAssertTrue(note.canSend)
        await note.send()
        XCTAssertNil(note.error)
        XCTAssertEqual(note.trace, "hDEMO0000001")
        XCTAssertFalse(note.canSend, "one note per sheet")
        let got = await hab.frictionNotes
        XCTAssertEqual(got, [FrictionParams(blocker: true, pane: "%1", session: "s1", text: "The diff view could not show the whole file.")])
        await fleet.setActive(false)
    }

    func testTooLongAndNoCapability() async {
        let (fleet, _, model) = await setup(friction: false)
        XCTAssertFalse(model.context.takesFriction, "no menu item on a hab without the capability")
        let note = FrictionNote(session: "s1", pane: "%1", connection: model.connection)
        note.text = String(repeating: "x", count: FrictionNote.maxText + 1)
        XCTAssertFalse(note.canSend)
        note.text = "fine"
        await note.send()
        XCTAssertNil(note.trace)
        XCTAssertNotNil(note.error, "a hab without diag.friction refuses it")
        await fleet.setActive(false)
    }
}
