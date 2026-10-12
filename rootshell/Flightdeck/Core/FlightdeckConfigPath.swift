//
//  FlightdeckConfigPath.swift
//  FlightDeck
//
//  The text config file under FlightDeck's name: ~/.config/flightdeck/config
//  on the Standalone Mac app, Documents/flightdeck.conf in sandboxed builds.
//  A user who already has upstream's rootshell file keeps using it until a
//  flightdeck file exists. Nothing here moves, copies or deletes a file.
//

import Foundation

nonisolated enum FlightdeckConfigPath {
    /// The file to read and write in place of upstream's `stock` path.
    static func url(stock: URL) -> URL {
        guard let renamed = renamed(stock) else { return stock }
        let fm = FileManager.default
        if !fm.fileExists(atPath: renamed.path), fm.fileExists(atPath: stock.path) {
            return stock
        }
        return renamed
    }

    /// Upstream's display path, renamed when `active` is the flightdeck file.
    static func shellPath(stock: String, active: URL) -> String {
        guard isRenamed(active) else { return stock }
        return stock
            .replacingOccurrences(of: "/rootshell/config", with: "/flightdeck/config")
            .replacingOccurrences(of: "rootshell.conf", with: "flightdeck.conf")
    }

    /// `.../rootshell/config` becomes `.../flightdeck/config`, and
    /// `.../rootshell.conf` becomes `.../flightdeck.conf`. Any other path has
    /// no flightdeck name.
    static func renamed(_ stock: URL) -> URL? {
        let parent = stock.deletingLastPathComponent()
        if stock.lastPathComponent == "rootshell.conf" {
            return parent.appendingPathComponent("flightdeck.conf")
        }
        if stock.path.hasSuffix("/rootshell/config") {
            return parent.deletingLastPathComponent()
                .appendingPathComponent("flightdeck", isDirectory: true)
                .appendingPathComponent("config")
        }
        return nil
    }

    private static func isRenamed(_ url: URL) -> Bool {
        url.lastPathComponent == "flightdeck.conf" || url.path.hasSuffix("/flightdeck/config")
    }
}
