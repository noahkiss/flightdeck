import SwiftUI

/// The composer's Return key on a hardware keyboard (app.md 4.2), as in Messages on the Mac:
/// Return sends, Shift-Return and Option-Return start a new line where the caret is.
enum ComposerKeys {
    enum Return: Equatable {
        case send
        case newline
    }

    static func returnKey(_ modifiers: EventModifiers) -> Return {
        modifiers.contains(.shift) || modifiers.contains(.option) ? .newline : .send
    }

    /// The text with a line break in place of the selection (at the end with none), and the
    /// caret after it.
    static func insertingNewline(_ text: String, at selection: TextSelection?) -> (String, TextSelection) {
        var out = text
        var range = out.endIndex..<out.endIndex
        if case .selection(let r)? = selection?.indices,
           r.lowerBound >= out.startIndex, r.upperBound <= out.endIndex {
            range = r
        }
        let offset = out.distance(from: out.startIndex, to: range.lowerBound)
        out.replaceSubrange(range, with: "\n")
        let caret = out.index(out.startIndex, offsetBy: offset + 1)
        return (out, TextSelection(insertionPoint: caret))
    }
}
