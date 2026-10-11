//
//  GhosttyConfig.swift
//  rootshell
//
//  Wrapper around ghostty_config_t for iOS
//

import Foundation
import SwiftUI
import Combine
import os
import GhosttyKit

extension Ghostty {
    /// Wrapper around ghostty_config_t
    class Config: ObservableObject {
        // The underlying C pointer to the Ghostty config structure
        private(set) var config: ghostty_config_t? = nil {
            didSet {
                guard let previous = oldValue else { return }
                // The old pointer may still be queued for delivery to the core
                // app and its surfaces. Free it behind those calls, not ahead
                // of them.
                nonisolated(unsafe) let old = previous
                Ghostty.TerminalView.ghosttyAPIQueue.async { ghostty_config_free(old) }
            }
        }

        /// True if the configuration is loaded
        var loaded: Bool { config != nil }

        /// Return the errors found while loading the configuration
        var errors: [String] {
            guard let cfg = self.config else { return [] }

            var diags: [String] = []
            let diagsCount = ghostty_config_diagnostics_count(cfg)
            for i in 0..<diagsCount {
                let diag = ghostty_config_get_diagnostic(cfg, UInt32(i))
                let message = String(cString: diag.message)
                diags.append(message)
            }

            return diags
        }

        init() {
            if let cfg = Self.loadConfig() {
                self.config = cfg
            }
        }

        init(clone config: ghostty_config_t) {
            self.config = ghostty_config_clone(config)
        }

        deinit {
            self.config = nil
        }

        /// Initializes a new configuration and loads all the values
        static private func loadConfig() -> ghostty_config_t? {
            guard let cfg = ghostty_config_new() else {
                logger.critical("ghostty_config_new failed")
                return nil
            }

            // On iOS, we don't load from files by default
            // Configuration will be managed via UserDefaults/ConfigStore
            // For now, just use defaults
            ghostty_config_finalize(cfg)

            // Log any configuration errors
            let diagsCount = ghostty_config_diagnostics_count(cfg)
            if diagsCount > 0 {
                logger.warning("config error: \(diagsCount) configuration errors")
                for i in 0..<diagsCount {
                    let diag = ghostty_config_get_diagnostic(cfg, UInt32(i))
                    let message = String(cString: diag.message)
                    logger.warning("config error: \(message)")
                }
            }

            return cfg
        }

        // MARK: - Configuration Values

        /// Get a configuration value by key
        func getString(_ key: String) -> String? {
            guard let config = self.config else { return nil }
            var v: UnsafePointer<Int8>? = nil
            guard ghostty_config_get(config, &v, key, UInt(key.count)) else { return nil }
            guard let ptr = v else { return nil }
            return String(cString: ptr)
        }

        func getBool(_ key: String, defaultValue: Bool = false) -> Bool {
            guard let config = self.config else { return defaultValue }
            var v = defaultValue
            _ = ghostty_config_get(config, &v, key, UInt(key.count))
            return v
        }

        func getInt(_ key: String, defaultValue: Int = 0) -> Int {
            guard let config = self.config else { return defaultValue }
            var v: CInt = CInt(defaultValue)
            _ = ghostty_config_get(config, &v, key, UInt(key.count))
            return Int(v)
        }

        func getDouble(_ key: String, defaultValue: Double = 0.0) -> Double {
            guard let config = self.config else { return defaultValue }
            var v: Double = defaultValue
            _ = ghostty_config_get(config, &v, key, UInt(key.count))
            return v
        }

        // Common configuration accessors for iOS
        var fontFamily: String? {
            getString("font-family")
        }

        var fontSize: Int {
            getInt("font-size", defaultValue: 13)
        }

        var theme: String? {
            getString("theme")
        }

        var backgroundColor: String? {
            getString("background")
        }

        var foregroundColor: String? {
            getString("foreground")
        }

        var backgroundOpacity: Double {
            getDouble("background-opacity", defaultValue: 1.0)
        }

        // MARK: - Theme Management

        /// Set the theme by creating a config file and reloading
        /// - Parameter themeName: The name of the theme (e.g., "Catppuccin Mocha")
        /// - Returns: true if the theme was set successfully, false otherwise
        func setTheme(_ themeName: String) -> Bool {
            logger.info("Setting theme to: \(themeName)")

            // Write config file with theme setting
            guard writeConfigFile(themeName: themeName) else {
                logger.error("Failed to write config file for theme: \(themeName)")
                return false
            }

            // Create a new config and load it
            guard let newConfig = Self.loadConfigWithTheme() else {
                logger.error("Failed to load config with theme: \(themeName)")
                return false
            }

            // Replace our config with the new one
            self.config = newConfig

            logger.info("Theme set successfully: \(themeName)")
            return true
        }

        /// Set the font size by creating a config file and reloading
        /// - Parameter size: The font size in points (e.g., 13)
        /// - Returns: true if the font size was set successfully, false otherwise
        func setFontSize(_ size: Int) -> Bool {
            logger.info("Setting font size to: \(size)")

            // Write config file with font size setting
            guard writeConfigFile(fontSize: size) else {
                logger.error("Failed to write config file for font size: \(size)")
                return false
            }

            // Create a new config and load it
            guard let newConfig = Self.loadConfigWithTheme() else {
                logger.error("Failed to load config with font size: \(size)")
                return false
            }

            // Replace our config with the new one
            self.config = newConfig

            logger.info("Font size set successfully: \(size)")
            return true
        }

        /// Set the font family by creating a config file and reloading
        /// - Parameter family: The font family name (e.g., "FiraCode Nerd Font Mono")
        /// - Returns: true if the font family was set successfully, false otherwise
        func setFontFamily(_ family: String) -> Bool {
            logger.info("Setting font family to: \(family)")

            // Write config file with font family setting
            guard writeConfigFile(fontFamily: family) else {
                logger.error("Failed to write config file for font family: \(family)")
                return false
            }

            // Create a new config and load it
            guard let newConfig = Self.loadConfigWithTheme() else {
                logger.error("Failed to load config with font family: \(family)")
                return false
            }

            // Replace our config with the new one
            self.config = newConfig

            logger.info("Font family set successfully: \(family)")
            return true
        }

        /// Get the config directory path (~/.config/ghostty on iOS)
        private static var configDirectory: URL? {
            // On iOS, use Application Support directory
            guard let appSupport = FileManager.default.urls(
                for: .applicationSupportDirectory,
                in: .userDomainMask
            ).first else {
                return nil
            }

            let ghosttyDir = appSupport.appendingPathComponent("ghostty")

            // Create directory if it doesn't exist
            try? FileManager.default.createDirectory(
                at: ghosttyDir,
                withIntermediateDirectories: true
            )

            return ghosttyDir
        }

        private static func effectiveTransparencySettings() -> (opacity: Double, blur: Int) {
#if targetEnvironment(macCatalyst)
            let transparencyManager = TransparencyManager.shared
            // Glass styles blur via an NSGlassEffectView window behind the
            // terminal, so the CGS radius must be 0. The renderer still draws
            // the theme background at `opacity`; the glass shows through it.
            return (
                opacity: transparencyManager.effectiveBackgroundOpacity,
                blur: transparencyManager.usesGlass ? 0 : Int(transparencyManager.backgroundBlurRadius)
            )
#else
            // Glass is a SwiftUI layer over the backdrop; there is no radius blur.
            return (opacity: TransparencyManager.shared.effectiveBackgroundOpacity, blur: 0)
#endif
        }

        /// Over the Theme Gradient backdrop, OSC 11 reports the composited color.
        static func backdropConfigLines(themeName: String) -> [String] {
            EffectiveSurface.ghosttyConfigLines(
                backgroundHex: ThemeManager.shared.themeInfo(for: themeName)?.colors.background
            )
        }

        /// Write a config file with the specified theme, font size, and/or font family
        private func writeConfigFile(themeName: String? = nil, fontSize: Int? = nil, fontFamily: String? = nil) -> Bool {
            guard let configDir = Self.configDirectory else {
                logger.error("Failed to get config directory")
                return false
            }

            let configFile = configDir.appendingPathComponent("config")

            // Build config content with available settings
            var configLines: [String] = []

            if let theme = themeName {
                configLines.append("theme = \(theme)")
            } else {
                // Preserve current theme from saved preferences
                let currentTheme = ThemeManager.shared.currentTheme
                configLines.append("theme = \(currentTheme)")
            }

            if let size = fontSize {
                configLines.append("font-size = \(size)")
            } else {
                // Preserve current font size from saved preferences
                let currentSize = Int(FontManager.shared.currentFontSize)
                configLines.append("font-size = \(currentSize)")
            }

            // Font family - only write if explicitly set (nil = use Ghostty default)
            if let family = fontFamily {
                configLines.append("font-family = \(family)")
            } else if let currentFamily = FontManager.shared.currentFontFamily {
                // Preserve current font family from saved preferences
                configLines.append("font-family = \(currentFamily)")
            }
            // If no font family is set, don't write font-family to use Ghostty's default

            // Mac Catalyst: Spawn shell directly instead of via /usr/bin/login
            // /usr/bin/login has issues with PTY setup in Catalyst (no job control)
            #if targetEnvironment(macCatalyst)
            // Honours the Local Shell setting; falls back to $SHELL -l when unset.
            let shellCommand = LocalShellSettings.ghosttyConfigCommand
            configLines.append("command = \(shellCommand)")

            // Set initial working directory to home
            let homeDir = NSHomeDirectory()
            configLines.append("working-directory = \(homeDir)")

            logger.info("Catalyst config: command=\"\(shellCommand)\", working-directory=\"\(homeDir)\"")
            #endif

            // Always include clipboard paste safety setting
            configLines.append("clipboard-paste-bracketed-safe-newline = true")

            // Enable OSC 52 clipboard access for terminal applications (e.g., neovim, tmux)
            // This allows programs to read/write the system clipboard via escape sequences
            #if FLIGHTDECK
            // flightdeck: programs may write the clipboard but never read it.
            configLines.append("clipboard-read = deny")
            #else
            configLines.append("clipboard-read = allow")
            #endif
            configLines.append("clipboard-write = allow")

            // Auto-copy selected text to clipboard (default on, matches macOS Ghostty)
            // `primary` is the selection clipboard, which writeClipboard treats as
            // copy-on-select; upstream maps `true` to the standard clipboard.
            let copyOnSelect = SettingsStore.shared.value(Settings.Selection.copyOnSelect)
            configLines.append("copy-on-select = \(copyOnSelect ? "primary" : "none")")

            // Option key as Alt setting (matches Ghostty's macos-option-as-alt)
            let optionAsAlt = SettingsStore.shared.value(Settings.Keyboard.optionKeyAsAlt)
            if optionAsAlt == .on {
                configLines.append("macos-option-as-alt = true")
            } else if optionAsAlt == .left {
                configLines.append("macos-option-as-alt = left")
            } else if optionAsAlt == .right {
                configLines.append("macos-option-as-alt = right")
            } else {
                configLines.append("macos-option-as-alt = false")
            }

            // Font ligatures (contextual alternates and standard ligatures)
            let fontManager = FontManager.shared
            if fontManager.ligaturesEnabled {
                configLines.append("font-feature = calt")
                configLines.append("font-feature = liga")
            } else {
                // Disable ligatures (Ghostty documentation: "-calt, -liga, -dlig")
                configLines.append("font-feature = -calt")
                configLines.append("font-feature = -liga")
                configLines.append("font-feature = -dlig")
            }

            // Per-font stylistic set / feature toggles from FontManager
            let featureLines = fontManager.fontFeatureConfigLines()
            configLines.append(contentsOf: featureLines)

            // Per-font cell box adjustments (adjust-cell-width / -height)
            configLines.append(contentsOf: fontManager.cellAdjustmentConfigLines())

            configLines.append("font-thicken = true")
            configLines.append(contentsOf: SelectionManager.shared.generateSelectionConfigLines())
            configLines.append(contentsOf: CursorManager.shared.generateCursorConfigLines())
            configLines.append(contentsOf: PaletteManager.shared.generatePaletteConfigLines())

            // Transparency is only supported on Mac Catalyst. Keep iPad/iOS opaque
            // so the Ghostty surface matches the surrounding themed SwiftUI background.
            let transparency = Self.effectiveTransparencySettings()
            configLines.append("background-opacity = \(transparency.opacity)")
            configLines.append("background-blur = \(transparency.blur)")
            configLines.append(contentsOf: Self.backdropConfigLines(themeName: themeName ?? ThemeManager.shared.currentTheme))

            // Window padding for text inset from edges (background still renders to edges).
            // Padding balance stays off so the terminal grid remains pinned during
            // live window resize; any sub-cell remainder stays on the trailing edges.
            let windowPadding = PaddingManager.shared.configPadding()
            configLines.append("window-padding-x = \(windowPadding.x)")
            configLines.append("window-padding-y = \(windowPadding.y)")
            configLines.append("window-padding-balance = \(windowPadding.balance)")

            // Keybinds from KeybindManager (terminal actions only)
            let keybindLines = KeybindManager.shared.terminalKeybindConfigLines()
            configLines.append(contentsOf: keybindLines)

            // Custom shader configuration
            let shaderLines = ShaderManager.shared.generateConfigLines()
            configLines.append(contentsOf: shaderLines)

            let configContent = configLines.joined(separator: "\n") + "\n"

            do {
                try configContent.write(to: configFile, atomically: true, encoding: .utf8)
                logger.info("Wrote config file: \(configFile.path)")
                return true
            } catch {
                logger.error("Failed to write config file: \(error)")
                return false
            }
        }

        /// Load a new config with theme from the config file
        private static func loadConfigWithTheme() -> ghostty_config_t? {
            guard let cfg = ghostty_config_new() else {
                logger.critical("ghostty_config_new failed")
                return nil
            }

            // Load from default files (will read our config file)
            ghostty_config_load_default_files(cfg)

            // Finalize the config
            ghostty_config_finalize(cfg)

            // Log any configuration errors
            let diagsCount = ghostty_config_diagnostics_count(cfg)
            if diagsCount > 0 {
                logger.warning("config error: \(diagsCount) configuration errors")
                for i in 0..<diagsCount {
                    let diag = ghostty_config_get_diagnostic(cfg, UInt32(i))
                    let message = String(cString: diag.message)
                    logger.warning("config error: \(message)")
                }
            }

            return cfg
        }

        // MARK: - Per-Surface Theme Configuration

        /// Create a config with a specific theme for per-surface overrides
        /// This writes a temporary config file, loads it, and returns the config.
        /// The caller is responsible for applying this config to a specific surface.
        /// - Parameter themeName: The theme name to use
        /// - Returns: A ghostty_config_t configured with the specified theme, or nil on failure
        static func createConfigForTheme(_ themeName: String) -> ghostty_config_t? {
            logger.info("Creating per-surface config for theme: \(themeName)")

            // Write config file with the override theme
            guard writeConfigFileForTheme(themeName: themeName) else {
                logger.error("Failed to write config file for per-surface theme: \(themeName)")
                return nil
            }

            // Load the config
            guard let cfg = loadConfigWithTheme() else {
                logger.error("Failed to load config for per-surface theme: \(themeName)")
                return nil
            }

            logger.info("Created per-surface config for theme: \(themeName)")
            return cfg
        }

        /// Write a config file with the specified theme (static version for per-surface configs)
        /// Uses current font settings from FontManager
        private static func writeConfigFileForTheme(themeName: String) -> Bool {
            guard let configDir = configDirectory else {
                logger.error("Failed to get config directory")
                return false
            }

            let configFile = configDir.appendingPathComponent("config")

            // Build config content
            var configLines: [String] = []

            // Use the specified theme
            configLines.append("theme = \(themeName)")

            // Preserve current font size from saved preferences
            let currentSize = Int(FontManager.shared.currentFontSize)
            configLines.append("font-size = \(currentSize)")

            // Font family - preserve current if set
            if let currentFamily = FontManager.shared.currentFontFamily {
                configLines.append("font-family = \(currentFamily)")
            }

            // Mac Catalyst: Spawn shell directly instead of via /usr/bin/login
            #if targetEnvironment(macCatalyst)
            configLines.append("command = \(LocalShellSettings.ghosttyConfigCommand)")
            let homeDir = NSHomeDirectory()
            configLines.append("working-directory = \(homeDir)")
            #endif

            // Standard settings
            configLines.append("clipboard-paste-bracketed-safe-newline = true")

            // Enable OSC 52 clipboard access for terminal applications (e.g., neovim, tmux)
            #if FLIGHTDECK
            // flightdeck: programs may write the clipboard but never read it.
            configLines.append("clipboard-read = deny")
            #else
            configLines.append("clipboard-read = allow")
            #endif
            configLines.append("clipboard-write = allow")

            // Auto-copy selected text to clipboard (default on, matches macOS Ghostty)
            // `primary` is the selection clipboard, which writeClipboard treats as
            // copy-on-select; upstream maps `true` to the standard clipboard.
            let copyOnSelect = SettingsStore.shared.value(Settings.Selection.copyOnSelect)
            configLines.append("copy-on-select = \(copyOnSelect ? "primary" : "none")")

            // Option key as Alt setting (matches Ghostty's macos-option-as-alt)
            let optionAsAlt = SettingsStore.shared.value(Settings.Keyboard.optionKeyAsAlt)
            if optionAsAlt == .on {
                configLines.append("macos-option-as-alt = true")
            } else if optionAsAlt == .left {
                configLines.append("macos-option-as-alt = left")
            } else if optionAsAlt == .right {
                configLines.append("macos-option-as-alt = right")
            } else {
                configLines.append("macos-option-as-alt = false")
            }

            // Font ligatures
            let fontManager = FontManager.shared
            if fontManager.ligaturesEnabled {
                configLines.append("font-feature = calt")
                configLines.append("font-feature = liga")
            } else {
                configLines.append("font-feature = -calt")
                configLines.append("font-feature = -liga")
                configLines.append("font-feature = -dlig")
            }

            // Per-font stylistic set / feature toggles from FontManager
            let featureLines = fontManager.fontFeatureConfigLines()
            configLines.append(contentsOf: featureLines)

            // Per-font cell box adjustments (adjust-cell-width / -height)
            configLines.append(contentsOf: fontManager.cellAdjustmentConfigLines())

            configLines.append("font-thicken = true")
            configLines.append(contentsOf: SelectionManager.shared.generateSelectionConfigLines())
            configLines.append(contentsOf: CursorManager.shared.generateCursorConfigLines())
            configLines.append(contentsOf: PaletteManager.shared.generatePaletteConfigLines())

            let transparency = Self.effectiveTransparencySettings()
            configLines.append("background-opacity = \(transparency.opacity)")
            configLines.append("background-blur = \(transparency.blur)")
            configLines.append(contentsOf: backdropConfigLines(themeName: themeName))

            // Window padding for text inset from edges (background still renders to edges).
            // Keep this in sync with app config generation above.
            let windowPadding = PaddingManager.shared.configPadding()
            configLines.append("window-padding-x = \(windowPadding.x)")
            configLines.append("window-padding-y = \(windowPadding.y)")
            configLines.append("window-padding-balance = \(windowPadding.balance)")

            // Keybinds
            let keybindLines = KeybindManager.shared.terminalKeybindConfigLines()
            configLines.append(contentsOf: keybindLines)

            // Custom shader configuration
            let shaderLines = ShaderManager.shared.generateConfigLines()
            configLines.append(contentsOf: shaderLines)

            let configContent = configLines.joined(separator: "\n") + "\n"

            do {
                try configContent.write(to: configFile, atomically: true, encoding: .utf8)
                return true
            } catch {
                logger.error("Failed to write config file for per-surface theme: \(error)")
                return false
            }
        }
    }
}
