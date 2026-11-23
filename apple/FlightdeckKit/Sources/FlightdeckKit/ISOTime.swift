import Foundation
import Synchronization

/// RFC 3339 times as the hab writes them, with or without a fraction. One
/// formatter of each kind, made once: cards and usage rows parse times on
/// every redraw. ISO8601DateFormatter is not Sendable, so the mutex owns them.
enum ISOTime {
    private struct Formatters {
        let fractional: ISO8601DateFormatter
        let whole: ISO8601DateFormatter
    }

    private static let formatters = Mutex(Formatters(
        fractional: make([.withInternetDateTime, .withFractionalSeconds]),
        whole: make([.withInternetDateTime])
    ))

    private static func make(_ options: ISO8601DateFormatter.Options) -> ISO8601DateFormatter {
        let f = ISO8601DateFormatter()
        f.formatOptions = options
        return f
    }

    /// The time in `s`, with a fraction or without; nil when it is no RFC 3339 time.
    static func parse(_ s: String) -> Date? {
        formatters.withLock { $0.fractional.date(from: s) ?? $0.whole.date(from: s) }
    }

    /// `t` in UTC with milliseconds: `2026-11-18T11:06:40.123Z`.
    static func format(_ t: Date) -> String {
        formatters.withLock { $0.fractional.string(from: t) }
    }
}
