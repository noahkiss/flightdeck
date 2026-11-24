import XCTest
@testable import FlightdeckKit

/// A session window's customisable toolbar (app.md 3.13): one id per window kind, fixed items,
/// and controls that turn off instead of leaving.
final class SessionToolbarTests: XCTestCase {
    func testEachWindowKindHasItsOwnID() {
        XCTAssertEqual(SessionToolbar.id(capcom: true), "session.capcom")
        XCTAssertEqual(SessionToolbar.id(capcom: false), "session.agent")
    }

    func testItemsAreFixedPerKind() {
        XCTAssertEqual(SessionToolbar.items(capcom: true).map(\.rawValue),
                       ["view", "new-conversation", "voice", "console", "show-in-chat", "files", "friction"])
        XCTAssertEqual(SessionToolbar.items(capcom: false).map(\.rawValue),
                       ["voice", "console", "show-in-chat", "files", "friction"])
    }

    /// What used to leave the toolbar now turns off: friction before the hab says it takes
    /// notes, the switch and console on the Capcom service, console on a console.
    func testControlsTurnOffInsteadOfLeaving() {
        var c = ChatContext()
        XCTAssertFalse(SessionToolbar.enabled(.friction, context: c, session: true, show: true))
        c.takesFriction = true
        XCTAssertTrue(SessionToolbar.enabled(.friction, context: c, session: true, show: true))
        XCTAssertTrue(SessionToolbar.enabled(.console, context: c, session: true, show: true))
        XCTAssertFalse(SessionToolbar.enabled(.console, context: c, session: false, show: true))
        XCTAssertFalse(SessionToolbar.enabled(.showInChat, context: c, session: true, show: false))
        var console = c
        console.isConsole = true
        XCTAssertFalse(SessionToolbar.enabled(.console, context: console, session: true, show: true))
        var service = c
        service.isCapcom = true
        service.isCapcomService = true
        for item in [SessionToolbar.Item.view, .console, .showInChat] {
            XCTAssertFalse(SessionToolbar.enabled(item, context: service, session: true, show: true), item.rawValue)
        }
        XCTAssertTrue(SessionToolbar.enabled(.newConversation, context: service, session: true, show: true))
    }
}
