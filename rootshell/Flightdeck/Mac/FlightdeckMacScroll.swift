//
//  FlightdeckMacScroll.swift
//  FlightDeck
//
//  Terminal scrolling on the Mac, in the mouse-captured path (tmux, zellij,
//  vim), where upstream sends wheel and trackpad deltas to Ghostty itself.
//
//  Ghostty reads a precise scroll delta as pixels and scrolls one row per
//  cell height of pixels. Native Ghostty on macOS sends `scrollingDelta x 2`,
//  which on a Retina Mac is one delta pixel per screen pixel. Upstream sends
//  `translation x 2` in UIKit points. That matched only while the terminal
//  rendered at 2 px per point; at the screen-resolution scale
//  (FlightdeckMacText) it would scroll 30% faster. Multiplying by the
//  terminal's own content scale keeps one delta pixel per terminal pixel at
//  either scale, and equals upstream's 2 when the scale is 2.
//
//  Upstream's own momentum decays to 1% of its speed in one second, far
//  shorter than macOS momentum (UIScrollView's normal rate, 0.998 per ms,
//  which keeps 13.5% after one second). On the Mac the decay follows macOS.
//

import UIKit

extension FlightdeckHooks {
    /// `handleCatalystScrollGesture`: the multiplier from UIKit points to
    /// Ghostty's precise-scroll pixels.
    static func catalystScrollGain(_ view: UIView) -> CGFloat {
        view.contentScaleFactor > 0 ? view.contentScaleFactor : 2
    }

    /// The captured-scroll momentum decay per second.
    nonisolated static func scrollMomentumDecay(stock: CGFloat) -> CGFloat {
        #if targetEnvironment(macCatalyst)
        return 0.135
        #else
        return stock
        #endif
    }
}
