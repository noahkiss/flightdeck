//
//  FlightdeckBrand.swift
//  rootshell
//
//  flightdeck: the app's user-facing name, for text that upstream writes as a
//  literal. It reads the containing app's CFBundleDisplayName, which
//  FLIGHTDECK_DISPLAY_NAME sets in Configuration/Flightdeck.xcconfig.
//  PushNotificationService/FlightdeckBrand.swift is the same file for the push
//  extension; keep the two in step.
//

import Foundation

enum FlightdeckBrand {
    /// "FlightDeck" in flightdeck builds.
    static let name: String = {
        // From an extension, climb out of the .appex to the containing app.
        var url = Bundle.main.bundleURL
        while url.pathExtension != "app", url.pathComponents.count > 1 {
            url.deleteLastPathComponent()
        }
        let bundle = Bundle(url: url) ?? Bundle.main
        return bundle.object(forInfoDictionaryKey: "CFBundleDisplayName") as? String
            ?? bundle.object(forInfoDictionaryKey: "CFBundleName") as? String
            ?? "FlightDeck"
    }()
}
