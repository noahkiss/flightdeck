import Foundation

/// The link a Live Activity opens: `flightdeck://request?hab=<host>&pane=<pane>&request=<id>&session=<id>`.
/// The app opens the approval sheet at that request, or the oldest open one.
public enum RequestLink {
    public static func url(hab: String, pane: String?, request: String?, session: String?) -> URL {
        var c = URLComponents()
        c.scheme = "flightdeck"
        c.host = "request"
        c.queryItems = [URLQueryItem(name: "hab", value: hab)]
            + [("pane", pane), ("request", request), ("session", session)].compactMap { k, v in v.map { URLQueryItem(name: k, value: $0) } }
        return c.url!
    }

    /// Reads a link back into the push object it stands for; nil for any other link.
    public static func meta(_ url: URL) -> PushMeta? {
        guard url.scheme == "flightdeck", url.host == "request",
              let items = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems,
              let hab = items.first(where: { $0.name == "hab" })?.value
        else { return nil }
        let v = { (k: String) in items.first { $0.name == k }?.value }
        return PushMeta(kind: "permission", hab: hab, session: v("session"), pane: v("pane"), request: v("request"))
    }
}
