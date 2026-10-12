//
//  FlightdeckSceneRoot.swift
//  FlightDeck
//
//  The modifier on the root view of each app window. Features that apply to
//  a whole window add themselves here, so they need no new upstream line.
//

import SwiftUI

extension FlightdeckHooks {
    /// `RootShellApp.body`, on the root view of the main and visor windows.
    struct SceneRoot: ViewModifier {
        func body(content: Content) -> some View {
            content.modifier(HideFocusRing())
        }
    }
}

/// Hides the keyboard focus ring on the Mac. Focus still moves with the
/// keyboard; only the drawn ring goes.
private struct HideFocusRing: ViewModifier {
    func body(content: Content) -> some View {
        #if targetEnvironment(macCatalyst)
        content.focusEffectDisabled()
        #else
        content
        #endif
    }
}
