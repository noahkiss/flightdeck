import Foundation

/// Turns a command's output bytes into the plain lines a card shows (app.md
/// 6.1). It is not a terminal: it drops escape sequences, keeps line breaks,
/// and applies carriage return, backspace, tab and erase-to-end-of-line on
/// the current line, so a progress line shows its last state. What a program
/// drew on the alternate screen is left out; the terminal view showed it.
public enum TerminalText {
    /// A private-use scalar that stands for "erase to the end of the line"
    /// between the two passes.
    private static let eraseLine: Unicode.Scalar = "\u{E000}"

    public static func lines(_ data: Data) -> [String] {
        let text = String(decoding: strip(data), as: UTF8.self)
        var lines: [[Character]] = []
        var line: [Character] = []
        var col = 0
        func put(_ c: Character) {
            if col < line.count {
                line[col] = c
            } else {
                line.append(contentsOf: repeatElement(" ", count: col - line.count))
                line.append(c)
            }
            col += 1
        }
        for s in text.unicodeScalars {
            switch s {
            case "\n":
                lines.append(line)
                line = []
                col = 0
            case "\r":
                col = 0
            case "\u{08}":
                col = max(0, col - 1)
            case "\t":
                let next = (col / 8 + 1) * 8
                while col < next { put(" ") }
            case eraseLine:
                if col < line.count { line.removeSubrange(col...) }
            default:
                if s.value < 0x20 || s.value == 0x7f { continue }
                put(Character(s))
            }
        }
        if !line.isEmpty { lines.append(line) }
        return lines.map { String($0).replacingOccurrences(of: "\\s+$", with: "", options: .regularExpression) }
    }

    /// Drops escape sequences and the alternate screen's bytes; keeps text
    /// and C0 controls. `CSI K` becomes `eraseLine`.
    static func strip(_ data: Data) -> Data {
        enum S { case ground, esc, csi, str, strEsc }
        var out = Data()
        out.reserveCapacity(data.count)
        var st = S.ground
        var csiParams: [UInt8] = []
        var alternate = false
        for b in data {
            if b == 0x18 || b == 0x1a {
                st = .ground
                continue
            }
            switch st {
            case .ground:
                if b == 0x1b {
                    st = .esc
                } else if !alternate {
                    out.append(b)
                }
            case .esc:
                switch b {
                case UInt8(ascii: "["):
                    st = .csi
                    csiParams = []
                case UInt8(ascii: "]"), UInt8(ascii: "P"), UInt8(ascii: "X"), UInt8(ascii: "^"), UInt8(ascii: "_"):
                    st = .str
                case 0x1b:
                    break
                case 0x20...0x2f:
                    break // an intermediate: the escape goes on
                default:
                    st = .ground
                }
            case .csi:
                if (0x40...0x7e).contains(b) {
                    let p = String(decoding: csiParams, as: UTF8.self)
                    if p.hasPrefix("?"), b == UInt8(ascii: "h") || b == UInt8(ascii: "l") {
                        let modes = p.dropFirst().split(separator: ";")
                        if modes.contains(where: { $0 == "1049" || $0 == "1047" || $0 == "47" }) {
                            alternate = b == UInt8(ascii: "h")
                        }
                    } else if b == UInt8(ascii: "K"), p.isEmpty || p == "0", !alternate {
                        out.append(contentsOf: String(eraseLine).utf8)
                    }
                    st = .ground
                } else if b == 0x1b {
                    st = .esc
                } else if csiParams.count < 64 {
                    csiParams.append(b)
                }
            case .str:
                if b == 0x07 {
                    st = .ground
                } else if b == 0x1b {
                    st = .strEsc
                }
            case .strEsc:
                st = b == UInt8(ascii: "\\") ? .ground : .str
            }
        }
        return out
    }
}
