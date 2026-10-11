//
//  UpdateManager.swift
//  rootshell
//
//  Sparkle 2 auto-update manager for Mac Catalyst Standalone builds only.
//  This file is excluded from App Store builds via EXCLUDED_SOURCE_FILE_NAMES.
//

import Foundation
import os
import Combine

#if STANDALONE && targetEnvironment(macCatalyst)
import Sparkle
#endif

/// Manages application updates via Sparkle framework.
/// Only active on Mac Catalyst Standalone builds.
@MainActor
final class UpdateManager: ObservableObject {

    static let shared = UpdateManager()

    private nonisolated static let logger = Logger(subsystem: "com.kk2.rootshell", category: "UpdateManager")

    #if STANDALONE && targetEnvironment(macCatalyst)
    /// The Sparkle updater controller
    private var updaterController: SPUStandardUpdaterController?

    /// Published state for UI binding
    @Published private(set) var canCheckForUpdates: Bool = false

    /// Whether Sparkle should automatically check for updates on a schedule
    @Published private(set) var automaticallyChecksForUpdates: Bool = false

    /// How often Sparkle checks for updates
    @Published private(set) var updateCheckInterval: UpdateCheckInterval = .daily

    /// Cancellable for KVO observation
    private var cancellables = Set<AnyCancellable>()

    /// Supported update check intervals
    enum UpdateCheckInterval: TimeInterval, CaseIterable, Identifiable {
        case hourly = 3600
        case everySixHours = 21600
        case daily = 86400
        case weekly = 604800

        var id: TimeInterval { rawValue }

        var displayName: String {
            switch self {
            case .hourly: return String(localized: "Every Hour", comment: "Update interval: hourly")
            case .everySixHours: return String(localized: "Every 6 Hours", comment: "Update interval: every 6 hours")
            case .daily: return String(localized: "Daily", comment: "Update interval: daily")
            case .weekly: return String(localized: "Weekly", comment: "Update interval: weekly")
            }
        }

        init(seconds: TimeInterval) {
            self = Self.allCases.min(by: { abs($0.rawValue - seconds) < abs($1.rawValue - seconds) }) ?? .daily
        }
    }
    #endif

    private init() {
        #if FLIGHTDECK
        return
        #endif
        #if STANDALONE && targetEnvironment(macCatalyst)
        setupSparkle()
        #endif
    }

    #if STANDALONE && targetEnvironment(macCatalyst)
    private func setupSparkle() {
        // SPUStandardUpdaterController handles the standard Sparkle UI
        // startingUpdater: true means it will auto-check on launch per SUScheduledCheckInterval
        updaterController = SPUStandardUpdaterController(
            startingUpdater: true,
            updaterDelegate: nil,
            userDriverDelegate: nil
        )

        // Seed defaults on first launch — Sparkle won't auto-check unless
        // SUEnableAutomaticChecks has been explicitly written to UserDefaults.
        let defaults = UserDefaults.standard
        if defaults.object(forKey: "SUEnableAutomaticChecks") == nil {
            defaults.set(true, forKey: "SUEnableAutomaticChecks")
        }
        if defaults.object(forKey: "SUScheduledCheckInterval") == nil {
            defaults.set(UpdateCheckInterval.daily.rawValue, forKey: "SUScheduledCheckInterval")
        }

        // Bind published properties to the updater's state
        if let updater = updaterController?.updater {
            canCheckForUpdates = updater.canCheckForUpdates
            automaticallyChecksForUpdates = updater.automaticallyChecksForUpdates
            updateCheckInterval = UpdateCheckInterval(seconds: updater.updateCheckInterval)

            // Observe changes using KVO publishers
            updater.publisher(for: \.canCheckForUpdates)
                .receive(on: DispatchQueue.main)
                .sink { [weak self] value in
                    self?.canCheckForUpdates = value
                }
                .store(in: &cancellables)

            updater.publisher(for: \.automaticallyChecksForUpdates)
                .receive(on: DispatchQueue.main)
                .sink { [weak self] value in
                    self?.automaticallyChecksForUpdates = value
                }
                .store(in: &cancellables)

            updater.publisher(for: \.updateCheckInterval)
                .receive(on: DispatchQueue.main)
                .sink { [weak self] value in
                    self?.updateCheckInterval = UpdateCheckInterval(seconds: value)
                }
                .store(in: &cancellables)
        }

        Self.logger.info("Sparkle updater initialized")
    }
    #endif

    /// Check for updates manually.
    /// This is the action for "Check for Updates..." menu item.
    func checkForUpdates() {
        #if STANDALONE && targetEnvironment(macCatalyst)
        guard let updater = updaterController?.updater else {
            Self.logger.warning("Updater not available")
            return
        }

        Self.logger.info("Manual update check triggered")
        updater.checkForUpdates()
        #else
        Self.logger.debug("Update check called on non-Sparkle build (no-op)")
        #endif
    }

    #if STANDALONE && targetEnvironment(macCatalyst)
    /// Set whether Sparkle automatically checks for updates.
    /// Uses explicit setter to avoid KVO infinite loops from `didSet`.
    func setAutomaticallyChecksForUpdates(_ enabled: Bool) {
        updaterController?.updater.automaticallyChecksForUpdates = enabled
    }

    /// Set the update check interval.
    /// Uses explicit setter to avoid KVO infinite loops from `didSet`.
    func setUpdateCheckInterval(_ interval: UpdateCheckInterval) {
        updaterController?.updater.updateCheckInterval = interval.rawValue
    }
    #endif

    /// Check if Sparkle is available (for UI purposes)
    var isSparkleAvailable: Bool {
        #if STANDALONE && targetEnvironment(macCatalyst)
        return updaterController != nil
        #else
        return false
        #endif
    }
}
