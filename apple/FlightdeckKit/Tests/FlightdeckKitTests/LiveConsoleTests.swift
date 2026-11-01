import CoreGraphics
import Foundation
import HabAPI
import SwiftUI
import XCTest
@testable import FlightdeckKit

/// The console against a real gff-hab with a holder. Skipped unless
/// `FLIGHTDECK_LIVE_SOCKET` names the hab's socket. The hab's `$SHELL` must
/// send prompt marks: start gff-hab with the console's shell setup
/// (`ConsoleShellSetup`) in its shell's startup file, for example through
/// `ZDOTDIR`. `FLIGHTDECK_LIVE_PROJECT_DIR` files the session in that
/// repository's project, so the console starts there.
///
/// It opens a session's console as the app does, runs `ls`, `false`,
/// `sleep 2` through the input line, then `vim`, and checks the cards and
/// the hand-over to the terminal. `FLIGHTDECK_SNAPSHOT_DIR` saves the cards
/// as a PNG.
@MainActor
final class LiveConsoleTests: XCTestCase {
    private var env: [String: String] { ProcessInfo.processInfo.environment }

    func testLiveConsole() async throws {
        guard let path = env["FLIGHTDECK_LIVE_SOCKET"], !path.isEmpty else {
            throw XCTSkip("FLIGHTDECK_LIVE_SOCKET not set")
        }
        let conn = HabConnection(id: "console", transport: UnixSocketTransport(path: path), timing: liveTiming(), clientLabel: "test-console")
        await conn.start()
        defer { Task { await conn.stop() } }
        await eventually("online", timeout: 10) { await conn.state.status == .online }

        let name = "live-\(UInt32.random(in: 0...UInt32.max))"
        let dir = env["FLIGHTDECK_LIVE_PROJECT_DIR"].flatMap { $0.isEmpty ? nil : $0 }
        let made = try await conn.call(HabMethods.sessionCreate, SessionCreateParams(argv: ["/bin/sh", "-c", "while :; do sleep 60; done"], name: name, projectDir: dir))
        await eventually("session", timeout: 5) { await conn.state.store.sessions[made.session] != nil }

        let pane = try await ConsoleOpener.open(session: made.session, calls: .init(connection: conn))
        let again = try await ConsoleOpener.open(session: made.session, calls: .init(connection: conn))
        XCTAssertEqual(again, pane, "a second open finds the running console")

        let m = ConsoleModel(pane: pane, connection: conn)
        let t = try XCTUnwrap(m.terminal)
        t.containerChanged(CGSize(width: 900, height: 600))
        t.start()
        m.start()
        defer {
            m.stop()
            t.stop()
        }
        await eventually("loaded", timeout: 10) { m.loaded }
        try await Task.sleep(for: .milliseconds(500)) // the first prompt

        for (i, line) in ["ls", "false", "sleep 2"].enumerated() {
            m.draft = line
            m.send()
            await eventually("card \(i + 1) ends", timeout: 10) { m.list.cards.count == i + 1 && m.list.cards[i].command.state != .running }
        }
        let cards = m.list.cards
        XCTAssertEqual(cards.map(\.title), ["ls", "false", "sleep 2"])
        XCTAssertEqual(cards.map(\.status), ["Exit 0", "Exit 1", "Exit 0"])
        let slept = try XCTUnwrap(cards[2].duration)
        XCTAssertGreaterThanOrEqual(slept, 2.0)
        XCTAssertLessThan(slept, 3.0)
        if let dir {
            let want = URL(fileURLWithPath: dir).resolvingSymlinksInPath().path
            XCTAssertEqual(cards[0].command.cwd.map { URL(fileURLWithPath: $0).resolvingSymlinksInPath().path }, want)
        }
        m.toggle(cards[0].id)
        await eventually("ls output", timeout: 5) { m.list.card(cards[0].id)?.output != nil }
        XCTAssertFalse(m.list.card(cards[0].id)?.output?.isEmpty ?? true, "ls printed the folder")
        print("live console cards:\n" + ConsoleRender.text(m.list))
        saveCards(m, "console-live-cards")

        // A full-screen program hands over to the terminal, and back.
        m.run("vim -u NONE -N")
        await eventually("terminal view", timeout: 10) { m.showsTerminal && m.fullScreenProgram }
        m.sendRaw("\u{1B}:q!\r")
        await eventually("cards again", timeout: 10) { !m.showsTerminal }
        await eventually("vim card", timeout: 10) { m.list.cards.count == 4 && m.list.cards[3].command.state == .done }
        XCTAssertEqual(m.list.cards[3].status, "Exit 0")

        _ = try? await conn.call(HabMethods.paneClose, PaneRef(pane: pane))
        _ = try? await conn.call(HabMethods.paneClose, PaneRef(pane: made.pane))
    }

    private func liveTiming() -> HabTiming {
        var t = HabTiming()
        t.callTimeout = .seconds(10)
        return t
    }

    private func saveCards(_ m: ConsoleModel, _ name: String) {
        #if os(macOS)
        guard let dir = env["FLIGHTDECK_SNAPSHOT_DIR"], !dir.isEmpty else { return }
        let view = VStack(alignment: .leading, spacing: 8) {
            ForEach(m.list.cards) { c in
                CommandCardView(card: c, expanded: m.expanded.contains(c.id)) {}
            }
        }
        .padding(12)
        .frame(width: 560)
        .background(Color.white)
        let r = ImageRenderer(content: view)
        r.scale = 2
        guard let img = r.cgImage else { return }
        let rep = NSBitmapImageRep(cgImage: img)
        try? rep.representation(using: .png, properties: [:])?.write(to: URL(fileURLWithPath: dir).appendingPathComponent(name + ".png"))
        #endif
    }
}
