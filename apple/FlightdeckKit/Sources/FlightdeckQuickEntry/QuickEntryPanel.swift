#if os(macOS)
import AppKit
import FlightdeckKit
import KeyboardShortcuts
import SwiftUI

extension KeyboardShortcuts.Name {
    /// The quick entry shortcut (app.md 3.18). None is set at first: the owner picks it in
    /// Settings › This Mac.
    public static let quickEntry = Self("flightdeck.quickEntry")
}

/// Quick entry for Capcom (app.md 3.18): the system-wide shortcut and the floating panel. The
/// app makes one at launch; it lives as long as the app, with or without a window.
@MainActor
public final class QuickEntryController {
    public let entry: QuickEntry
    private var panel: QuickEntryPanel?
    /// The panel's place in the window count (app.md 12.1) and the shortcut's debounce.
    private let window: QuickEntryWindow
    /// Changes each time the panel shows, so the field takes the keyboard again.
    private var shown = 0

    public init(fleet: Fleet) {
        entry = QuickEntry(capcom: fleet.capcom)
        window = QuickEntryWindow(fleet: fleet, entry: entry)
        // A Carbon hot key: no Accessibility permission and no entitlement.
        KeyboardShortcuts.onKeyUp(for: .quickEntry) { [weak self] in self?.toggle() }
    }

    /// The shortcut: shows the panel, or closes it when it shows. A second fire within
    /// 150 ms is ignored (KeyboardShortcuts issue 243).
    public func toggle() {
        guard window.acceptsToggle() else { return }
        if panel?.isVisible == true { close() } else { show() }
    }

    /// Shows the panel over every app, without making Flightdeck the active app.
    public func show() {
        let p = panel ?? make()
        panel = p
        shown += 1
        p.contentViewController = host()
        place(p)
        window.shown()
        p.makeKeyAndOrderFront(nil)
    }

    /// Hides the panel. A send it started keeps the habs connected until it ends.
    public func close() {
        panel?.orderOut(nil)
        window.hidden()
    }

    private func host() -> NSViewController {
        let c = NSHostingController(rootView: QuickEntryView(entry: entry, shown: shown) { [weak self] in self?.close() })
        c.sizingOptions = [.preferredContentSize]
        c.view.wantsLayer = true
        c.view.layer?.backgroundColor = .clear
        return c
    }

    private func make() -> QuickEntryPanel {
        let p = QuickEntryPanel()
        // A click elsewhere closes it, as Spotlight's panel does, unless a send runs: then the
        // panel stays to show how the send ended.
        p.onResign = { [weak self] in
            guard let self, self.window.closesOnResign else { return }
            self.close()
        }
        p.onCancel = { [weak self] in self?.close() }
        return p
    }

    /// Centred across the screen with the pointer, in its upper third.
    private func place(_ p: NSPanel) {
        let mouse = NSEvent.mouseLocation
        guard let screen = NSScreen.screens.first(where: { NSMouseInRect(mouse, $0.frame, false) }) ?? NSScreen.main else { return }
        p.layoutIfNeeded()
        let size = p.contentViewController?.view.fittingSize ?? p.frame.size
        let v = screen.visibleFrame
        let top = v.maxY - v.height / 4
        p.setFrame(NSRect(x: v.midX - size.width / 2, y: top - size.height, width: size.width, height: size.height), display: true)
    }
}

/// A floating panel that takes the keyboard without activating the app.
final class QuickEntryPanel: NSPanel {
    var onResign: (() -> Void)?
    var onCancel: (() -> Void)?

    init() {
        super.init(contentRect: NSRect(x: 0, y: 0, width: 620, height: 120),
                   styleMask: [.nonactivatingPanel, .titled, .fullSizeContentView],
                   backing: .buffered, defer: true)
        isFloatingPanel = true
        level = .floating
        collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        titleVisibility = .hidden
        titlebarAppearsTransparent = true
        isMovableByWindowBackground = true
        hidesOnDeactivate = false
        isReleasedWhenClosed = false
        isOpaque = false
        backgroundColor = .clear
        hasShadow = true
        for b in [NSWindow.ButtonType.closeButton, .miniaturizeButton, .zoomButton] {
            standardWindowButton(b)?.isHidden = true
        }
        setAccessibilityLabel("Ask Capcom")
    }

    override var canBecomeKey: Bool { true }
    override var canBecomeMain: Bool { false }

    override func resignKey() {
        super.resignKey()
        onResign?()
    }

    /// Esc.
    override func cancelOperation(_ sender: Any?) {
        onCancel?()
    }
}

/// The quick entry rows of Settings › This Mac (app.md 3.18, 20.7).
public struct QuickEntrySettings: View {
    public init() {}

    public var body: some View {
        Section {
            KeyboardShortcuts.Recorder("Quick entry for Capcom", name: .quickEntry)
        } footer: {
            Text("Opens a panel from any app to send Capcom a message. It needs no special permission.")
        }
    }
}
#endif
