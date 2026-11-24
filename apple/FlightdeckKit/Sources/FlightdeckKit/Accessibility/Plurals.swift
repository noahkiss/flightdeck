import Foundation

// Counts in words (app.md 22.3). A count with a noun uses automatic grammar agreement:
// `^[\(n) file](inflect: true)` reads "1 file" or "3 files" in the owner's language. A count
// whose verb or whole phrase changes ("1 report waits", "Every minute") uses a plural
// variation in the package's String Catalog instead.

/// A string from the package's String Catalog with automatic grammar agreement applied.
/// `String(localized:)` leaves the `^[…](inflect: true)` markup as it is; the attributed
/// string resolves it.
func inflected(_ value: String.LocalizationValue) -> String {
    String(AttributedString(localized: value, bundle: #bundle).characters)
}

/// A string from the package's String Catalog, with its plural variations.
func localized(_ value: String.LocalizationValue) -> String {
    String(localized: value, bundle: #bundle)
}
