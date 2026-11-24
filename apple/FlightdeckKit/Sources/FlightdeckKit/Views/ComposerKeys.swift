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

    /// Whether a plain Return sends. Not with a Japanese, Chinese or Korean input method among
    /// the owner's keyboards: there Return commits the composed characters, and SwiftUI shows a
    /// key press no marked text to check, so a send could leave the composition behind. Those
    /// owners send with Cmd-Return or the button (app.md 4.2).
    static func returnSends(languages: [String]) -> Bool {
        !languages.contains { l in ["ja", "zh", "ko"].contains { l.lowercased().hasPrefix($0) } }
    }

    /// The languages of the owner's input methods: the enabled keyboards on iPhone and iPad, the
    /// preferred languages on the Mac.
    @MainActor
    static var inputLanguages: [String] {
        #if os(iOS)
        UITextInputMode.activeInputModes.compactMap(\.primaryLanguage)
        #else
        Locale.preferredLanguages
        #endif
    }

    /// The text with a line break in place of the selection (at the end with none), and the
    /// caret after it.
    static func insertingNewline(_ text: String, at selection: TextSelection?) -> (String, TextSelection) {
        var out = text
        var range = out.endIndex..<out.endIndex
        // A selection left from text the app rewrote (a send, dictation) may not fit this text:
        // then the line goes at the end.
        if case .selection(let r)? = selection?.indices,
           r.lowerBound >= out.startIndex, r.upperBound <= out.endIndex,
           let lo = String.Index(r.lowerBound, within: out), let hi = String.Index(r.upperBound, within: out) {
            range = lo..<hi
        }
        let offset = out.distance(from: out.startIndex, to: range.lowerBound)
        out.replaceSubrange(range, with: "\n")
        let caret = out.index(out.startIndex, offsetBy: offset + 1)
        return (out, TextSelection(insertionPoint: caret))
    }
}
