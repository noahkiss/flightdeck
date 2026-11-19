import Foundation

/// The key bar's Ctrl and Opt (app.md 5.5): one tap arms a modifier for the
/// next key, a second tap within 400 ms locks it, and a tap on a locked key or
/// a late tap on an armed one turns it off. The next key takes every active
/// modifier; armed ones then turn off and locked ones stay. A hardware
/// keyboard turns every modifier off.
public struct StickyModifiers: Equatable, Sendable {
    public enum Key: String, CaseIterable, Sendable {
        case ctrl
        case opt

        /// The label on the key.
        public var label: String {
            switch self {
            case .ctrl: "Ctrl"
            case .opt: "Opt"
            }
        }

        /// What VoiceOver calls the key.
        public var spokenName: String {
            switch self {
            case .ctrl: "Control"
            case .opt: "Option"
            }
        }
    }

    public enum State: String, Equatable, Sendable {
        case off
        case armed
        case locked

        /// What VoiceOver reads as the key's value.
        public var spoken: String {
            switch self {
            case .off: "Off"
            case .armed: "On for the next key"
            case .locked: "Locked"
            }
        }

        public var isActive: Bool { self != .off }
    }

    /// A second tap within this locks the key.
    public static let doubleTap: TimeInterval = 0.4

    public private(set) var ctrl: State = .off
    public private(set) var opt: State = .off
    private var lastTap: [Key: Date] = [:]

    public init() {}

    public subscript(_ k: Key) -> State {
        get {
            switch k {
            case .ctrl: ctrl
            case .opt: opt
            }
        }
        set {
            switch k {
            case .ctrl: ctrl = newValue
            case .opt: opt = newValue
            }
        }
    }

    public var anyActive: Bool { ctrl.isActive || opt.isActive }

    /// A tap on the key at `now`.
    public mutating func tap(_ k: Key, at now: Date = Date()) {
        let last = lastTap[k]
        switch self[k] {
        case .off:
            self[k] = .armed
        case .armed:
            if let last, now.timeIntervalSince(last) < Self.doubleTap {
                self[k] = .locked
            } else {
                self[k] = .off
            }
        case .locked:
            self[k] = .off
        }
        lastTap[k] = now
    }

    /// The next key went out with the active modifiers: armed ones turn off,
    /// locked ones stay. Returns the modifiers the key took.
    @discardableResult
    public mutating func consume() -> Set<Key> {
        var used: Set<Key> = []
        for k in Key.allCases where self[k].isActive {
            used.insert(k)
            if self[k] == .armed { self[k] = .off }
        }
        return used
    }

    /// A hardware keyboard was attached: its own modifier keys take over.
    public mutating func hardwareKeyboardAttached() {
        ctrl = .off
        opt = .off
        lastTap = [:]
    }

    /// Takes the state libghostty reports after it applied a key from the
    /// system keyboard (an armed key it spent), keeping the tap times.
    public mutating func sync(ctrl c: State, opt o: State) {
        ctrl = c
        opt = o
    }
}

/// The one-line hint the first time a modifier is armed on a device (app.md 5.5).
public enum StickyHint {
    public static func text(_ k: StickyModifiers.Key) -> String {
        "\(k.label) is on for the next key. Tap twice to lock it."
    }
}
