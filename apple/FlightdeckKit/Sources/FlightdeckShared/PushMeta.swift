import Foundation

/// The `flightdeck` object of a push: the small part the app reads before it
/// loads the rest (app.md 8.3, hab-logic-api.md 3.9 Push, Payload). Shared by
/// the app, the notification service extension and the widget extension, so
/// this module depends on Foundation only.
public struct PushMeta: Sendable, Equatable, Codable {
    public var v: Int
    /// `permission`, `question`, `auth`, `error`, `done`, `answered` or `notice`.
    public var kind: String
    /// The hab's host id, as its welcome names it.
    public var hab: String
    public var session: String?
    public var pane: String?
    public var state: String?
    /// The harness's request id; a permission answer must name it.
    public var request: String?
    /// RFC 3339; after it the hab refuses a remote answer.
    public var expiresAt: String?
    public var notice: String?
    /// The round and run of a round notification (rounds.md 10.2).
    public var round: String?
    public var run: String?
    /// The item "Ask an agent" hands to `capcom.open` as its one context item, unread
    /// (capcom.md 8.5). Present while some host runs Capcom.
    public var capcomContext: CapcomItem?
    /// The device id of the hab that runs Capcom: the one "Ask an agent" calls.
    public var capcomHome: String?

    /// A `capcom_context`: the same fields as the hab's `CapcomContextRef`.
    public struct CapcomItem: Sendable, Equatable, Codable {
        public var kind: String
        public var ref: String
        public var host: String?

        public init(kind: String, ref: String, host: String? = nil) {
            self.kind = kind
            self.ref = ref
            self.host = host
        }
    }

    enum CodingKeys: String, CodingKey {
        case v, kind, hab, session, pane, state, request
        case expiresAt = "expires_at"
        case notice, round, run
        case capcomContext = "capcom_context"
        case capcomHome = "capcom_home"
    }

    public init(v: Int = 1, kind: String, hab: String, session: String? = nil, pane: String? = nil, state: String? = nil, request: String? = nil, expiresAt: String? = nil, notice: String? = nil, capcomContext: CapcomItem? = nil, capcomHome: String? = nil, round: String? = nil, run: String? = nil) {
        self.v = v
        self.kind = kind
        self.hab = hab
        self.session = session
        self.pane = pane
        self.state = state
        self.request = request
        self.expiresAt = expiresAt
        self.notice = notice
        self.capcomContext = capcomContext
        self.capcomHome = capcomHome
        self.round = round
        self.run = run
    }

    /// The key under which a push and a local notification carry the object.
    public static let userInfoKey = "flightdeck"

    /// Reads the object from a notification's `userInfo`; nil when it has none
    /// or it does not parse.
    public init?(userInfo: [AnyHashable: Any]) {
        guard let raw = userInfo[Self.userInfoKey] else { return nil }
        guard JSONSerialization.isValidJSONObject(raw),
              let data = try? JSONSerialization.data(withJSONObject: raw),
              let m = try? JSONDecoder().decode(PushMeta.self, from: data)
        else { return nil }
        self = m
    }

    /// The object as a `userInfo` value, for a local notification.
    public var userInfoValue: [String: Any] {
        guard let data = try? JSONEncoder().encode(self),
              let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
        else { return [:] }
        return obj
    }

    public var expiry: Date? { expiresAt.flatMap(Self.parseDate) }

    /// Reads an RFC 3339 time, with or without fractional seconds.
    public static func parseDate(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime]
        return f.date(from: s)
    }
}

/// The expiry as a local clock time on a banner (app.md 16.1): the hab leaves
/// it out of the body and the notification service extension appends it.
public enum ExpiryText {
    /// `body` with " · Expires 14:05" appended, in the device's time zone and
    /// clock style. Unchanged with no expiry, or one already past.
    public static func append(to body: String, meta: PushMeta?, now: Date = Date(), locale: Locale = .current, timeZone: TimeZone = .current) -> String {
        guard let m = meta, m.kind == "permission", let at = m.expiry, at > now else { return body }
        let f = DateFormatter()
        f.locale = locale
        f.timeZone = timeZone
        f.dateStyle = .none
        f.timeStyle = .short
        // Past a day the time alone misleads; say the day too.
        if at.timeIntervalSince(now) > 20 * 3600 { f.dateStyle = .short }
        let text = "Expires " + f.string(from: at)
        return body.isEmpty ? text : body + " · " + text
    }
}
