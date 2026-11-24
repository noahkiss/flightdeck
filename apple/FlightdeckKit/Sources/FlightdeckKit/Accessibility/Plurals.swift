import Foundation
import Synchronization

// Counts in words (app.md 22.3). A count with a noun uses automatic grammar agreement:
// `^[\(n) file](inflect: true)` reads "1 file" or "3 files" in the owner's language. A count
// whose verb or whole phrase changes ("1 report waits", "Every minute") uses a plural
// variation in the package's String Catalog instead.

/// A string from the package's String Catalog with automatic grammar agreement applied.
/// `String(localized:)` leaves the `^[…](inflect: true)` markup as it is; the attributed
/// string resolves it. Resolving costs a Markdown parse, and the chat asks for the same few
/// counts on every render, so results are kept by the unresolved string and language.
func inflected(_ value: String.LocalizationValue) -> String {
    let key = InflectKey(raw: String(localized: value, bundle: #bundle), language: Locale.current.identifier)
    if let hit = inflectCache.withLock({ $0[key] }) { return hit }
    let out = String(AttributedString(localized: value, bundle: #bundle).characters)
    inflectCache.withLock { cache in
        if cache.count >= 512 { cache.removeAll() }
        cache[key] = out
    }
    return out
}

private struct InflectKey: Hashable, Sendable {
    let raw: String
    let language: String
}

private let inflectCache = Mutex<[InflectKey: String]>([:])

/// A string from the package's String Catalog, with its plural variations.
func localized(_ value: String.LocalizationValue) -> String {
    String(localized: value, bundle: #bundle)
}
