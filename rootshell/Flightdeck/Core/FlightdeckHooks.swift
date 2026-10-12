//
//  FlightdeckHooks.swift
//  FlightDeck
//
//  Every entry point that an upstream file calls. Each upstream call site is
//  one line that ends in a FLIGHTDECK-HOOK marker, and
//  scripts/flightdeck-hooks.json lists them all; CI runs
//  `scripts/flightdeck-hooks.py --check`.
//
//  A hook holds no logic of its own. With no feature registered it does
//  nothing or returns the stock value, so a removed feature leaves its
//  upstream line harmless.
//

import Foundation

enum FlightdeckHooks {
    /// `RootShellApp.init`, after the protected-data guard.
    @MainActor
    static func launch() {
        FlightdeckBootstrap.start()
    }
}
