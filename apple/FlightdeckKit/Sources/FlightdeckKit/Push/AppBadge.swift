import Foundation

/// The app badge (app.md 3.1, 7.9): the board's needs-you count on the app
/// icon (iPhone and iPad) or the Dock tile (Mac).
///
/// - The app is the only writer: pushes carry no badge (app.md 8.7).
/// - It writes only while the app is in front with at least one hab online,
///   or with no habs at all. In the background every connection closes
///   (app.md 12.1), so the count would read 0; the badge keeps the count from
///   the last time the app was in front instead.
/// - It writes only when the count changes, and again after a write the
///   platform refused (no notification permission yet).
@MainActor
public final class AppBadge {
    /// Sets the badge; returns false when the platform refused it.
    public typealias Write = @MainActor (Int) async -> Bool

    private let write: Write
    /// The count the badge shows; nil before the first write.
    public private(set) var shown: Int?
    private var wanted: Int?
    private var writing = false

    public init(write: @escaping Write) {
        self.write = write
    }

    /// Whether the count may be written now.
    nonisolated public static func writes(active: Bool, online: Int, habs: Int) -> Bool {
        active && (online > 0 || habs == 0)
    }

    /// The Dock tile's label: empty at 0.
    nonisolated public static func label(_ count: Int) -> String {
        count > 0 ? String(count) : ""
    }

    /// The board's count changed, or the app came to the front.
    public func update(_ count: Int, active: Bool, online: Int, habs: Int) {
        guard Self.writes(active: active, online: online, habs: habs) else { return }
        wanted = max(0, count)
        flush()
    }

    /// Writes the wanted count once, then again while it changed meanwhile.
    private func flush() {
        guard !writing, let n = wanted, n != shown else { return }
        writing = true
        Task { @MainActor in
            let ok = await write(n)
            writing = false
            if ok { shown = n } else if wanted == n { wanted = nil }
            flush()
        }
    }

    /// Waits until no write is on its way (tests).
    func settle() async {
        while writing { await Task.yield() }
    }
}
