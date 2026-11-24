import SwiftUI

extension View {
    /// A control that opens and folds a row: VoiceOver reads "expanded" or "collapsed" after its
    /// label, as it does for a system disclosure group (app.md 22.1).
    func disclosureState(_ open: Bool) -> some View {
        accessibilityValue(open ? Text("Expanded", bundle: #bundle) : Text("Collapsed", bundle: #bundle))
    }
}
