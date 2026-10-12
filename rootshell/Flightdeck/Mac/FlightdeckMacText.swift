//
//  FlightdeckMacText.swift
//  FlightDeck
//
//  Sharper terminal text on the Mac.
//
//  1. Ghostty's own default for `font-thicken` is off; upstream forces it on.
//     On the Mac it now follows a setting that defaults to off.
//  2. The Mac app runs in Catalyst's "Scaled to match iPad" mode: one UIKit
//     point covers 0.77 Mac points. Upstream renders the terminal at the
//     display scale (2 px per UIKit point on a Retina Mac), and the window
//     server then shrinks every 2 px to 1.54 px, which blurs glyph edges.
//     Rendering at displayScale x 0.77 gives one drawable pixel per screen
//     pixel. Ghostty derives its font DPI from the same content scale, so
//     glyphs keep their size and the grid keeps its rows and columns.
//

import SwiftUI
import UIKit

enum FlightdeckMacText {
    static let thickenKey = "flightdeckFontThicken"
    static let pixelGridKey = "flightdeckMacPixelGrid"
    /// Catalyst's iPad-idiom scale: a UIKit point drawn at 77% of a Mac point.
    static let iPadIdiomScale: CGFloat = 0.77

    static var thicken: Bool {
        UserDefaults.standard.bool(forKey: thickenKey)
    }

    static var matchesPixelGrid: Bool {
        UserDefaults.standard.object(forKey: pixelGridKey) as? Bool ?? true
    }

    /// The terminal's content scale for a display scale and interface idiom.
    static func contentScale(displayScale: CGFloat, isMac: Bool, isPadIdiom: Bool, matchPixelGrid: Bool) -> CGFloat {
        guard isMac, isPadIdiom, matchPixelGrid else { return displayScale }
        return displayScale * iPadIdiomScale
    }
}

extension FlightdeckHooks {
    /// `GhosttyConfig`: the `font-thicken` line. On the Mac it follows the
    /// setting; elsewhere upstream's line stands.
    nonisolated static func fontThickenLine(stock: String) -> String {
        #if targetEnvironment(macCatalyst)
        return "font-thicken = \(UserDefaults.standard.bool(forKey: FlightdeckMacText.thickenKey))"
        #else
        return stock
        #endif
    }

    /// `TerminalView.setupView`: the terminal's content scale.
    static func terminalContentScale(_ displayScale: CGFloat) -> CGFloat {
        #if targetEnvironment(macCatalyst)
        let isMac = true
        #else
        let isMac = false
        #endif
        return FlightdeckMacText.contentScale(
            displayScale: displayScale,
            isMac: isMac,
            isPadIdiom: UIDevice.current.userInterfaceIdiom == .pad,
            matchPixelGrid: FlightdeckMacText.matchesPixelGrid
        )
    }

    /// `FontSettingsView`: FlightDeck's Mac text settings.
    @ViewBuilder
    static func fontSettingsRows() -> some View {
        #if targetEnvironment(macCatalyst)
        FlightdeckMacTextRows()
        #endif
    }
}

#if targetEnvironment(macCatalyst)
private struct FlightdeckMacTextRows: View {
    @AppStorage(FlightdeckMacText.thickenKey) private var thicken = false
    @AppStorage(FlightdeckMacText.pixelGridKey) private var pixelGrid = true

    var body: some View {
        Toggle("Thicken Font Strokes", isOn: $thicken)
            .onChange(of: thicken) { _, _ in
                // Rewrites the Ghostty config and pushes it to every
                // surface, the path upstream's ligature toggle uses.
                FontManager.shared.ligaturesDidChange.send(FontManager.shared.ligaturesEnabled)
            }
            .padding(.vertical, 4)
            .themedRow()

        VStack(alignment: .leading, spacing: 2) {
            Toggle("Render Text at Screen Resolution", isOn: $pixelGrid)
            Text("Applies to terminals opened after the change.")
                .font(.caption)
                .foregroundStyle(.secondary)
        }
        .padding(.vertical, 4)
        .themedRow()
    }
}
#endif
