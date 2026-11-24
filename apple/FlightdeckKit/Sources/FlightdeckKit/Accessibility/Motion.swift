import SwiftUI

/// Reduce Motion (app.md 22.4): an expand, a fold, a zoom or a scroll the app animates runs at
/// once while the setting is on. Views read `accessibilityReduceMotion` and pass the animation
/// through here.
func gateMotion(_ animation: Animation, reduce: Bool) -> Animation? {
    reduce ? nil : animation
}
