import Foundation
import HabAPI

/// Shared code for every target. Keep it free of AppKit and UIKit so both
/// platforms and `swift test` can use it.
public enum AppInfo {
    /// The on-device name, from the bundle's Info.plist.
    public static func displayName(_ bundle: Bundle = .main) -> String {
        bundle.object(forInfoDictionaryKey: "CFBundleDisplayName") as? String ?? "Flightdeck"
    }

    /// The gff-hab API revision this build speaks.
    public static let apiRevision = HabAPI.revision
}
