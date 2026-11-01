import Foundation

/// Follows a pane's output for the alternate screen (`CSI ?1049h`, `?1047h`,
/// `?47h` and their `l` forms), so the console can hand a full-screen
/// program to the terminal view and take it back (app.md 6.1). It knows only
/// how a CSI sequence is built, and keeps its state across chunks, so a
/// sequence split over any number of reads is found once.
public struct ScreenModeScanner: Sendable, Equatable {
    /// True while the pane shows the alternate screen.
    public private(set) var alternate = false

    private enum State: Sendable, Equatable {
        case ground
        case esc
        case csi(private: Bool)
    }

    private var state = State.ground
    private var params: [Int] = []
    private var current: Int?
    private var bad = false

    public init() {}

    /// Back to the main screen and the ground state: a snapshot draws a new terminal.
    public mutating func reset() {
        self = ScreenModeScanner()
    }

    public mutating func feed(_ data: Data) {
        for b in data { step(b) }
    }

    private mutating func step(_ b: UInt8) {
        if b == 0x18 || b == 0x1a { // CAN and SUB abort a sequence
            state = .ground
            return
        }
        switch state {
        case .ground:
            if b == 0x1b { state = .esc }
        case .esc:
            switch b {
            case UInt8(ascii: "["):
                state = .csi(private: false)
                params = []
                current = nil
                bad = false
            case UInt8(ascii: "c"):
                alternate = false // RIS
                state = .ground
            case 0x1b:
                break
            default:
                state = .ground
            }
        case .csi(let priv):
            switch b {
            case UInt8(ascii: "?") where params.isEmpty && current == nil && !priv:
                state = .csi(private: true)
            case UInt8(ascii: "0")...UInt8(ascii: "9"):
                let d = Int(b - UInt8(ascii: "0"))
                let v = (current ?? 0) * 10 + d
                current = v > 99_999 ? 99_999 : v
            case UInt8(ascii: ";"):
                params.append(current ?? 0)
                current = nil
            case 0x1b:
                state = .esc
            case 0x40...0x7e:
                if let c = current { params.append(c) }
                if priv, !bad, b == UInt8(ascii: "h") || b == UInt8(ascii: "l") {
                    if params.contains(where: { $0 == 1049 || $0 == 1047 || $0 == 47 }) {
                        alternate = b == UInt8(ascii: "h")
                    }
                }
                state = .ground
            case 0x20...0x2f, 0x3a...0x3f:
                bad = true // intermediates or other markers: not a plain private mode set
            default:
                break // C0 controls inside a CSI run and the CSI goes on
            }
        }
    }
}
