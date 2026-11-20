import Foundation

/// The trace ids the app sends on each request (diagnostics.md 5.1, 5.2): `a` for the app, then
/// 11 random Crockford base32 characters. A trace holds no content and no time; the hab logs it
/// on the request's failed-request line and passes it to a peer hab.
enum Trace {
    private static let alphabet = Array("0123456789ABCDEFGHJKMNPQRSTVWXYZ")

    /// A new app trace.
    static func new() -> String {
        var g = SystemRandomNumberGenerator()
        return "a" + String((0..<11).map { _ in alphabet[Int(g.next() & 31)] })
    }

    /// Whether `s` has a trace's form: a, g or h, then 11 upper-case Crockford characters.
    static func isValid(_ s: String) -> Bool {
        let c = Array(s)
        guard c.count == 12, "agh".contains(c[0]) else { return false }
        return c.dropFirst().allSatisfy { alphabet.contains($0) }
    }
}
