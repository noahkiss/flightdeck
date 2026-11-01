import Foundation
import HabAPI

/// A hab's pairing offer as a code (transport.md 6.1): the fields of the
/// API's `PairingOffer` (`devices.pair`) plus a label, from its QR code, its pairing
/// link or the same text pasted:
///
///     flightdeck://pair?v=1&id=SHA256:<43 chars>&ep=<host:port>&ep=...&t=<token>&l=<label>
///
/// - `v`: the format version, 1.
/// - `id`: the hab's device id, the SHA-256 fingerprint of its host key. The
///   app pins it.
/// - `ep`: the embedded server's endpoints, in the order to try; at least one.
///   `host`, `host:port` or `[v6]:port`; the port defaults to 22.
/// - `t`: the one-time token, 128 bits as unpadded base64url (22 characters).
/// - `l`: the hab's label, optional.
///
/// Values are percent-encoded as in any URL query. Unknown keys are ignored,
/// so a later hab can add some. A code that repeats `v`, `id`, `t` or `l`
/// is refused: it is malformed or spliced from two offers.
public struct PairingCode: Sendable, Equatable {
    public var version: Int
    public var deviceID: String
    public var endpoints: [SSHEndpoint]
    public var token: Data
    public var label: String?

    public static let scheme = "flightdeck"

    /// The most endpoints a code may list, so a crafted code cannot fan out
    /// dozens of dials.
    public static let maxEndpoints = 8

    public init(version: Int = 1, deviceID: String, endpoints: [SSHEndpoint], token: Data, label: String? = nil) {
        self.version = version
        self.deviceID = deviceID
        self.endpoints = endpoints
        self.token = token
        self.label = label
    }

    /// The code for an offer from `devices.pair`. Nil when a field does not
    /// fit the code: an endpoint that is not host:port, or a token that is
    /// not 128 bits of base64url.
    public init?(offer: PairingOffer, label: String? = nil) {
        guard SSHFingerprint.isValid(offer.deviceId), let token = Self.decodeBase64url(offer.token), token.count == 16 else { return nil }
        var eps: [SSHEndpoint] = []
        for e in offer.endpoints {
            guard let ep = SSHEndpoint.parse(e) else { return nil }
            eps.append(ep)
        }
        guard !eps.isEmpty else { return nil }
        self.init(deviceID: offer.deviceId, endpoints: eps, token: token, label: label)
    }

    /// The offer as a link, the inverse of `parse`.
    public var link: String {
        var c = URLComponents()
        c.scheme = Self.scheme
        c.host = "pair"
        var items = [URLQueryItem(name: "v", value: String(version)), URLQueryItem(name: "id", value: deviceID)]
        items += endpoints.map { URLQueryItem(name: "ep", value: $0.description) }
        items.append(URLQueryItem(name: "t", value: Self.base64url(token)))
        if let label { items.append(URLQueryItem(name: "l", value: label)) }
        c.queryItems = items
        // URLComponents leaves ':' and '+' alone in a query; that is valid.
        return c.string ?? ""
    }

    /// Parses an offer from a scanned code or pasted text. Text around the
    /// link is ignored.
    public static func parse(_ text: String) throws -> PairingCode {
        guard let link = findLink(text), let c = URLComponents(string: link) else {
            throw PairingCodeError.notAnOffer
        }
        guard c.scheme?.lowercased() == scheme,
              (c.host?.lowercased() == "pair" || c.path.lowercased() == "pair" || c.path.lowercased() == "/pair")
        else {
            throw PairingCodeError.notAnOffer
        }
        let items = c.queryItems ?? []
        func one(_ k: String) -> String? { items.first { $0.name == k }?.value }
        // A key that must appear once and appears twice is a spliced or broken code.
        for k in ["v", "id", "t", "l"] where items.filter({ $0.name == k }).count > 1 {
            throw PairingCodeError.repeated(k)
        }

        guard let v = one("v") else { throw PairingCodeError.missing("v") }
        guard let version = Int(v) else { throw PairingCodeError.unsupportedVersion(v) }
        guard version == 1 else { throw PairingCodeError.unsupportedVersion(v) }

        guard let id = one("id"), !id.isEmpty else { throw PairingCodeError.missing("id") }
        guard SSHFingerprint.isValid(id) else { throw PairingCodeError.badDeviceID(id) }

        let eps = items.filter { $0.name == "ep" }.compactMap(\.value)
        guard !eps.isEmpty else { throw PairingCodeError.missing("ep") }
        guard eps.count <= maxEndpoints else { throw PairingCodeError.tooManyEndpoints(eps.count) }
        var endpoints: [SSHEndpoint] = []
        for e in eps {
            guard let ep = SSHEndpoint.parse(e) else { throw PairingCodeError.badEndpoint(e) }
            if !endpoints.contains(ep) { endpoints.append(ep) }
        }

        guard let t = one("t"), !t.isEmpty else { throw PairingCodeError.missing("t") }
        guard let token = decodeBase64url(t), token.count == 16 else { throw PairingCodeError.badToken }

        // No control or format characters (bidi overrides) in a label shown to the user.
        let shown = one("l").map { String(String.UnicodeScalarView($0.unicodeScalars.filter { !CharacterSet.controlCharacters.contains($0) && $0.properties.generalCategory != .format })) }
        var label = shown?.trimmingCharacters(in: .whitespacesAndNewlines)
        if label?.isEmpty ?? true { label = nil }
        if let l = label, l.count > 64 { label = String(l.prefix(64)) }

        return PairingCode(version: version, deviceID: id, endpoints: endpoints, token: token, label: label)
    }

    /// The first `flightdeck:` link in the text, up to the next whitespace.
    static func findLink(_ text: String) -> String? {
        guard let r = text.range(of: scheme + ":", options: .caseInsensitive) else { return nil }
        let rest = text[r.lowerBound...]
        let end = rest.firstIndex(where: { $0.isWhitespace || $0 == "<" || $0 == ">" || $0 == "\"" }) ?? rest.endIndex
        return String(rest[..<end])
    }

    static func base64url(_ d: Data) -> String {
        d.base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }

    static func decodeBase64url(_ s: String) -> Data? {
        guard s.allSatisfy({ $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") }) else { return nil }
        var b = s.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        while b.count % 4 != 0 { b += "=" }
        return Data(base64Encoded: b)
    }
}

public enum PairingCodeError: Error, Equatable, CustomStringConvertible {
    case notAnOffer
    case unsupportedVersion(String)
    case missing(String)
    case badDeviceID(String)
    case badEndpoint(String)
    case badToken
    case repeated(String)
    case tooManyEndpoints(Int)

    public var description: String {
        switch self {
        case .notAnOffer: return "This is not a Flightdeck pairing code."
        case .unsupportedVersion(let v): return "This pairing code uses format \(v). Update the app."
        case .missing(let k): return "The pairing code has no \(Self.name(k))."
        case .badDeviceID(let s): return "The hab id \"\(s)\" is not a SHA-256 fingerprint."
        case .badEndpoint(let s): return "The address \"\(s)\" is not host or host:port."
        case .badToken: return "The pairing token is not 128 bits of base64url."
        case .repeated(let k): return "The pairing code gives its \(Self.name(k)) more than once."
        case .tooManyEndpoints(let n): return "The pairing code lists \(n) addresses; at most \(PairingCode.maxEndpoints) are allowed."
        }
    }

    static func name(_ k: String) -> String {
        switch k {
        case "v": return "version"
        case "id": return "hab id"
        case "ep": return "address"
        case "t": return "token"
        case "l": return "label"
        default: return k
        }
    }
}
