#if os(macOS)
import AppKit
import SwiftUI
import XCTest
@testable import FlightdeckKit

/// The iPad and Mac split with a session open, laid out in a real hosting view.
@MainActor
final class BoardLayoutTests: XCTestCase {
    final class Frames {
        var list: CGRect = .zero
        var session: CGRect = .zero
    }

    private func layout(width: CGFloat) -> Frames {
        let frames = Frames()
        let root = BoardSessionSplit {
            Color.clear.background(GeometryReader { g in
                Color.clear.onAppear { frames.list = g.frame(in: .global) }
                    .onChange(of: g.size) { _, _ in frames.list = g.frame(in: .global) }
            })
        } session: {
            Color.clear.background(GeometryReader { g in
                Color.clear.onAppear { frames.session = g.frame(in: .global) }
                    .onChange(of: g.size) { _, _ in frames.session = g.frame(in: .global) }
            })
        }
        let host = NSHostingView(rootView: root)
        host.frame = CGRect(x: 0, y: 0, width: width, height: 600)
        let window = NSWindow(contentRect: host.frame, styleMask: [.borderless], backing: .buffered, defer: false)
        window.contentView = host
        host.layoutSubtreeIfNeeded()
        RunLoop.main.run(until: Date().addingTimeInterval(0.2))
        host.layoutSubtreeIfNeeded()
        return frames
    }

    func testMinimumWindowKeepsTheListInside() {
        let f = layout(width: 960)
        XCTAssertGreaterThanOrEqual(f.list.minX, 0, "the list's left edge is not clipped")
        XCTAssertGreaterThanOrEqual(f.list.width, BoardSessionSplit<EmptyView, EmptyView>.listMin)
        XCTAssertGreaterThanOrEqual(f.session.width, BoardSessionSplit<EmptyView, EmptyView>.sessionMin - 0.5)
        XCTAssertLessThanOrEqual(f.session.maxX, 960.5, "the session ends at the window edge")
    }

    func testWideWindowCapsTheList() {
        let f = layout(width: 1600)
        XCTAssertEqual(f.list.minX, 0, accuracy: 0.5)
        XCTAssertEqual(f.list.width, BoardSessionSplit<EmptyView, EmptyView>.listMax, accuracy: 0.5)
        XCTAssertEqual(f.session.maxX, 1600, accuracy: 0.5)
    }

    func testListWidthRule() {
        typealias S = BoardSessionSplit<EmptyView, EmptyView>
        XCTAssertEqual(S.listWidth(in: 960), 319)
        XCTAssertEqual(S.listWidth(in: 800), 300, "never under the minimum")
        XCTAssertEqual(S.listWidth(in: 2000), 380, "never over the maximum")
    }
}
#endif
