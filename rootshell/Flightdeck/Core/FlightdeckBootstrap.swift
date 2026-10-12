//
//  FlightdeckBootstrap.swift
//  FlightDeck
//
//  Starts FlightDeck features once per launch.
//

import Foundation

@MainActor
enum FlightdeckBootstrap {
    private static var didStart = false

    /// Runs once per launch, after protected data is available.
    static func start() {
        guard !didStart else { return }
        didStart = true
    }
}
