#if os(iOS)
import GameController
import GhosttyTerminal
import SwiftUI
import UIKit

/// One key on the terminal's key bar (app.md 5.5).
enum BarKey: Hashable, Identifiable {
    case esc, tab, left, down, up, right, paste
    case mod(StickyModifiers.Key)
    case text(String)

    var id: String {
        switch self {
        case .mod(let k): "mod-" + k.rawValue
        case .text(let t): "text-" + t
        default: String(describing: self)
        }
    }

    /// The bar, left to right: the row scrolls after the arrows.
    static let row: [BarKey] = [
        .esc, .tab, .mod(.ctrl), .mod(.opt), .left, .down, .up, .right,
        .text("|"), .text("/"), .text("~"), .text("-"), .text("_"), .text("`"), .paste,
    ]

    /// A text label, or nil for a key drawn as a symbol.
    var label: String? {
        switch self {
        case .esc: "Esc"
        case .tab: "Tab"
        case .mod(let k): k.label
        case .text(let t): t
        default: nil
        }
    }

    var systemImage: String? {
        switch self {
        case .left: "arrow.left"
        case .down: "arrow.down"
        case .up: "arrow.up"
        case .right: "arrow.right"
        case .paste: "doc.on.clipboard"
        default: nil
        }
    }

    /// What VoiceOver calls the key.
    var spokenName: String {
        switch self {
        case .esc: "Escape"
        case .tab: "Tab"
        case .left: "Left arrow"
        case .down: "Down arrow"
        case .up: "Up arrow"
        case .right: "Right arrow"
        case .paste: "Paste"
        case .mod(let k): k.spokenName
        case .text(let t):
            switch t {
            case "|": "Vertical bar"
            case "/": "Slash"
            case "~": "Tilde"
            case "-": "Hyphen"
            case "_": "Underscore"
            case "`": "Backtick"
            default: t
            }
        }
    }
}

extension TerminalModel {
    var platformView: UITerminalView? { view.attachedPlatformView }

    /// A tap on Ctrl or Opt. libghostty's sticky modifiers follow the bar's,
    /// so a key from the system keyboard takes them too.
    func tapModifier(_ k: StickyModifiers.Key) {
        keys.tap(k)
        pushKeys()
    }

    /// A key from the bar, with the active modifiers.
    func press(_ key: BarKey) {
        hookStickyKeys()
        guard let v = platformView else { return }
        switch key {
        case .mod(let k): tapModifier(k); return
        case .esc: v.sendKey(.escape)
        case .tab: v.sendKey(.tab)
        case .left: v.sendKey(.arrowLeft)
        case .down: v.sendKey(.arrowDown)
        case .up: v.sendKey(.arrowUp)
        case .right: v.sendKey(.arrowRight)
        case .text(let t): v.insertText(t)
        case .paste:
            guard let t = UIPasteboard.general.string, !t.isEmpty else { return }
            keys.consume()
            pushKeys()
            v.paste(text: t)
            return
        }
        // libghostty spent its armed modifiers on the key: read them back.
        syncKeysFromView()
    }

    /// A hardware keyboard was attached: every modifier turns off (app.md 5.5).
    func hardwareKeyboardAttached() {
        keys.hardwareKeyboardAttached()
        pushKeys()
    }

    /// Shows or hides the system keyboard.
    func toggleKeyboard() {
        guard let v = platformView else { return }
        if v.isFirstResponder {
            _ = v.resignFirstResponder()
        } else {
            v.acquireProgrammaticFocus()
        }
    }

    /// Sets libghostty's sticky state to the bar's.
    func pushKeys() {
        guard let v = platformView else { return }
        hookStickyKeys()
        pushingKeys = true
        v.resetStickyModifiers()
        for (k, m) in [(StickyModifiers.Key.ctrl, TerminalPublicStickyModifier.ctrl), (.opt, .alt)] {
            switch keys[k] {
            case .off: break
            case .armed: v.toggleStickyModifier(m)
            case .locked:
                // Two toggles in a row lock it, as a double tap does.
                v.toggleStickyModifier(m)
                v.toggleStickyModifier(m)
            }
        }
        pushingKeys = false
    }

    func hookStickyKeys() {
        guard let v = platformView else { return }
        v.setStickyModifierChangeHandler { [weak self] in self?.syncKeysFromView() }
    }

    private func syncKeysFromView() {
        guard !pushingKeys, let v = platformView else { return }
        func state(_ a: TerminalPublicStickyActivation) -> StickyModifiers.State {
            switch a {
            case .inactive: .off
            case .armed: .armed
            case .locked: .locked
            }
        }
        let c = state(v.stickyActivation(for: .ctrl))
        let o = state(v.stickyActivation(for: .alt))
        if c != keys.ctrl || o != keys.opt { keys.sync(ctrl: c, opt: o) }
    }
}

/// The key bar under the terminal (app.md 5.5): it stays while the system
/// keyboard is hidden; the terminal view hides it with a hardware keyboard.
struct TerminalKeyBar: View {
    let model: TerminalModel
    @AppStorage("FlightdeckModifierHintShown") private var hintShown = false
    @State private var hint: StickyModifiers.Key?
    @State private var keyboardShown = false

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            if let hint {
                Text(StickyHint.text(hint))
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                    .padding(.horizontal, 4)
                    .transition(.opacity)
            }
            HStack(spacing: 6) {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 6) {
                        ForEach(BarKey.row) { key in
                            keyButton(key)
                        }
                    }
                    .padding(.vertical, 1)
                }
                Button {
                    model.toggleKeyboard()
                } label: {
                    Image(systemName: keyboardShown ? "keyboard.chevron.compact.down" : "keyboard")
                        .font(.body.weight(.semibold))
                        .frame(minWidth: 44, minHeight: 44)
                        .background(keyFill, in: RoundedRectangle(cornerRadius: 10))
                }
                .buttonStyle(.plain)
                .accessibilityLabel(keyboardShown ? "Hide keyboard" : "Show keyboard")
            }
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 6)
        .frame(maxWidth: .infinity)
        .background(Color(uiColor: .secondarySystemBackground))
        .dynamicTypeSize(...DynamicTypeSize.accessibility1)
        .accessibilityElement(children: .contain)
        .accessibilityLabel("Terminal keys")
        .onChange(of: model.keys) { _, k in
            if !k.anyActive { withAnimation { hint = nil } }
        }
        .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardWillShowNotification)) { _ in keyboardShown = true }
        .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardWillHideNotification)) { _ in keyboardShown = false }
        .animation(.easeOut(duration: 0.15), value: model.keys)
        #if DEBUG
        .task { debugKeys() }
        #endif
    }

    private var keyFill: Color { Color(uiColor: .systemBackground) }

    @ViewBuilder private func keyButton(_ key: BarKey) -> some View {
        let state: StickyModifiers.State = {
            if case .mod(let k) = key { return model.keys[k] }
            return .off
        }()
        Button {
            if case .mod(let k) = key {
                model.tapModifier(k)
                if model.keys[k] == .armed, !hintShown {
                    hintShown = true
                    withAnimation { hint = k }
                }
            } else {
                model.press(key)
            }
        } label: {
            VStack(spacing: 3) {
                if let label = key.label {
                    Text(label)
                        .font(.subheadline.weight(.semibold))
                        .lineLimit(1)
                        .fixedSize()
                } else if let image = key.systemImage {
                    Image(systemName: image)
                        .font(.body.weight(.semibold))
                }
                if state == .locked {
                    Capsule()
                        .frame(width: 18, height: 3)
                        .accessibilityHidden(true)
                }
            }
            .padding(.horizontal, 10)
            .frame(minWidth: 44, minHeight: 44)
            .foregroundStyle(state == .locked ? Color.white : state == .armed ? Color.accentColor : Color.primary)
            .background(
                RoundedRectangle(cornerRadius: 10)
                    .fill(state == .locked ? Color.accentColor : state == .armed ? Color.accentColor.opacity(0.18) : keyFill)
            )
            .overlay(
                RoundedRectangle(cornerRadius: 10)
                    .strokeBorder(state == .armed ? Color.accentColor : .clear, lineWidth: 1.5)
            )
            .contentShape(RoundedRectangle(cornerRadius: 10))
        }
        .buttonStyle(.plain)
        .accessibilityLabel(key.spokenName)
        .modifier(ModifierAccessibility(key: key, state: state))
    }

    #if DEBUG
    /// Debug builds only: `-FlightdeckKeyBarState armed` or `locked` sets
    /// Ctrl at launch, for screenshots.
    private func debugKeys() {
        guard let s = UserDefaults.standard.string(forKey: "FlightdeckKeyBarState") else { return }
        Task { @MainActor in
            try? await Task.sleep(for: .seconds(2))
            model.tapModifier(.ctrl)
            if s == "armed", !hintShown {
                hintShown = true
                hint = .ctrl
            }
            if s == "locked" { model.tapModifier(.ctrl) }
        }
    }
    #endif
}

/// Ctrl and Opt tell VoiceOver their state and how to lock them.
private struct ModifierAccessibility: ViewModifier {
    let key: BarKey
    let state: StickyModifiers.State

    func body(content: Content) -> some View {
        if case .mod = key {
            content
                .accessibilityValue(state.spoken)
                .accessibilityHint("Tap for the next key. Tap twice to lock.")
                .accessibilityAddTraits(state.isActive ? .isSelected : [])
        } else {
            content
        }
    }
}

/// Whether a hardware keyboard is attached; the key bar gives way to one.
@MainActor
@Observable
final class HardwareKeyboard {
    static let shared = HardwareKeyboard()
    private(set) var attached = GCKeyboard.coalesced != nil
    private var observers: [NSObjectProtocol] = []

    private init() {
        let nc = NotificationCenter.default
        observers.append(nc.addObserver(forName: .GCKeyboardDidConnect, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { HardwareKeyboard.shared.attached = true }
        })
        observers.append(nc.addObserver(forName: .GCKeyboardDidDisconnect, object: nil, queue: .main) { _ in
            MainActor.assumeIsolated { HardwareKeyboard.shared.attached = GCKeyboard.coalesced != nil }
        })
    }
}
#endif
