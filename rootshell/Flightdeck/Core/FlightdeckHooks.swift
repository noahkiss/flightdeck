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

    // MARK: - Power

    /// `Settings.Power.maxRefreshRate`: prefer a battery-aware default in
    /// FlightDeck while preserving any explicit user setting.
    nonisolated static func powerRefreshRateDefault(
        stock: PowerManager.RefreshRateSetting
    ) -> PowerManager.RefreshRateSetting {
        FlightdeckPowerPolicy.defaultRefreshRate(stock: stock)
    }

    /// `Settings.Power.batteryRefreshRate`: prefer the saver tier for an
    /// unconfigured FlightDeck while preserving an explicit 60 Hz choice.
    nonisolated static func powerBatteryRefreshRateDefault(
        stock: PowerManager.BatteryRefreshRate
    ) -> PowerManager.BatteryRefreshRate {
        FlightdeckPowerPolicy.defaultBatteryRefreshRate(stock: stock)
    }

    // MARK: - zellij discovery

    /// `SessionDiscoveryCommand.command`: wraps upstream's zellij section so
    /// a zellij-nkmk host answers in JSON. A stock host runs `stock` as is.
    nonisolated static func zellijSessionsSection(stock: String, skipCaptures: Bool) -> String {
        ZellijForkDiscovery.section(stock: stock, skipCaptures: skipCaptures)
    }

    /// `SessionDiscoveryParser.parse`: the fork's sessions, or nil to let
    /// upstream's parser read the stock output.
    static func parseZellij(_ output: String) -> [ZellijSessionInfo]? {
        guard let sessions = ZellijForkDiscovery.parse(output) else { return nil }
        return sessions.map { session in
            ZellijSessionInfo(
                name: session.name,
                isAttached: session.isAttached,
                isExited: session.isExited,
                createdAgo: ZellijForkDiscovery.createdAgo(seconds: session.entry.createdSecondsAgo),
                capturedContent: session.capturedContent,
                flightdeck: session.extras
            )
        }
    }

    /// `MultiplexerSession.from(zellij:)`: the agent subtitle, or nil (stock).
    static func zellijSubtitle(_ session: ZellijSessionInfo) -> String? {
        guard let extras = session.flightdeck else { return nil }
        return ZellijForkDiscovery.subtitle(for: extras.agents)
    }

    // MARK: - Text config file

    /// `ConfigOverlayManager`: the config file under FlightDeck's name, or
    /// upstream's rootshell file while only that one exists.
    nonisolated static func configURL(stock: URL) -> URL {
        FlightdeckConfigPath.url(stock: stock)
    }

    /// `ConfigOverlayManager.shellDisplayPath`: the path Settings shows.
    nonisolated static func configShellPath(stock: String, active: URL) -> String {
        FlightdeckConfigPath.shellPath(stock: stock, active: active)
    }
}
