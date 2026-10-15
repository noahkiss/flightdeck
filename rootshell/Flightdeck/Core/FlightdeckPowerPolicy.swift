//
//  FlightdeckPowerPolicy.swift
//  FlightDeck
//
//  FlightDeck-specific defaults for upstream's power controls.
//

import Foundation

enum FlightdeckPowerPolicy {
    /// New installs follow the power source. Existing explicit choices are
    /// stored in UserDefaults and continue to override this registry default.
    nonisolated static func defaultRefreshRate(
        stock _: PowerManager.RefreshRateSetting
    ) -> PowerManager.RefreshRateSetting {
        .adaptive
    }
}
