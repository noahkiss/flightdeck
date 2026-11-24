import SwiftUI

extension View {
    /// The iPad pointer's highlight (app.md 3.8) on a control the system does not style: a
    /// plain-style button or a custom row. The system's own buttons, menus and list rows
    /// already react to the pointer; the Mac has no hover effect of this kind.
    func pointerHover() -> some View {
        #if os(iOS)
        hoverEffect()
        #else
        self
        #endif
    }
}
