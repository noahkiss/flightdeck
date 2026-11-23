import Foundation

/// Reopens the session a window last showed (app.md 3.12): the saved row id, opened once the
/// board lists that row, within `limit`. Any other open cancels it; the shell drops it then.
public struct SessionRestore: Equatable, Sendable {
    /// The saved row's id (`BoardRow.id`: hab and session).
    public var rowID: String
    /// When the window started waiting for the row.
    public var since: Date

    /// How long the window waits for the row, as New session does (app.md 3.10).
    public static let limit: TimeInterval = 60

    /// The scene storage value that means "the Capcom view" on the Mac.
    public static let capcom = "capcom"

    /// A restore of `saved`, or nil when the window had nothing open.
    public init?(saved: String, now: Date) {
        guard !saved.isEmpty else { return nil }
        rowID = saved
        since = now
    }

    /// Past the limit: the restore is over, whatever comes later.
    public func expired(now: Date) -> Bool {
        now.timeIntervalSince(since) > Self.limit
    }

    /// The row to open now, once the board lists it and while the restore runs.
    public func row(in rows: [BoardRow], now: Date) -> BoardRow? {
        guard !expired(now: now) else { return nil }
        return rows.first { $0.id == rowID }
    }
}
