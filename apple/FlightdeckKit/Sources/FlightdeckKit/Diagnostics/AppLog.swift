import Foundation
import os

/// The app's logger (diagnostics.md 4.10). Every line goes to the unified log;
/// `notice` and above also go to `app.log` once the app opened it.
///
/// A value in a line is private unless its kind says otherwise: a count or a
/// duration (an integer or a Bool), an enum word (`\(word: e)`), an id
/// Flightdeck minted (`\(id: pane)`), or an error's kind (`\(kind: error)`).
/// Host names, labels, paths and error descriptions stay private: the unified
/// log redacts them, and `app.log` never holds them. The source check in
/// `AppLogSourceTests` keeps `privacy: .public` and `Logger(` in this file only.
public struct AppLog: Sendable {
    public static let subsystem = "com.flightdeck.app"

    public let category: String
    private let logger: Logger
    private let file: AppLogFile? // nil: the shared file, once the app opened it

    public init(category: String) {
        self.init(category: category, file: nil)
    }

    init(category: String, file: AppLogFile?) {
        self.category = category
        self.file = file
        logger = Logger(subsystem: Self.subsystem, category: category)
    }

    public func debug(_ m: AppLogMessage) { emit(.debug, m) }
    public func info(_ m: AppLogMessage) { emit(.info, m) }
    public func notice(_ m: AppLogMessage) { emit(.notice, m) }
    public func error(_ m: AppLogMessage) { emit(.error, m) }
    public func fault(_ m: AppLogMessage) { emit(.fault, m) }

    private func emit(_ level: AppLogLevel, _ m: AppLogMessage) {
        let shown = m.redacted
        if m.hasPrivate {
            // The private values, whole, as the unified log keeps any `.private` value.
            logger.log(level: level.osType, "\(shown, privacy: .public) | \(m.full, privacy: .private)")
        } else {
            logger.log(level: level.osType, "\(shown, privacy: .public)")
        }
        if level >= .notice {
            (file ?? AppLogFile.shared)?.write(level, category: category, message: shown)
        }
    }
}

public enum AppLogLevel: Int, Comparable, Sendable {
    case debug, info, notice, error, fault

    public static func < (a: Self, b: Self) -> Bool { a.rawValue < b.rawValue }

    var word: String {
        switch self {
        case .debug: return "debug"
        case .info: return "info"
        case .notice: return "notice"
        case .error: return "error"
        case .fault: return "fault"
        }
    }

    var osType: OSLogType {
        switch self {
        case .debug: return .debug
        case .info: return .info
        case .notice: return .default
        case .error: return .error
        case .fault: return .fault
        }
    }
}

/// One log line: literal text and values, each value public or private by its
/// kind (see `AppLog`).
public struct AppLogMessage: ExpressibleByStringInterpolation, Sendable {
    struct Part: Sendable {
        let text: String
        let isPrivate: Bool
    }

    let parts: [Part]

    public init(stringLiteral value: String) {
        parts = [Part(text: value, isPrivate: false)]
    }

    public init(stringInterpolation: StringInterpolation) {
        parts = stringInterpolation.parts
    }

    /// The line with each private value replaced by `<private>`: what `app.log`
    /// holds and what the unified log shows by default.
    public var redacted: String { parts.map { $0.isPrivate ? "<private>" : $0.text }.joined() }

    /// The line with every value.
    public var full: String { parts.map(\.text).joined() }

    public var hasPrivate: Bool { parts.contains { $0.isPrivate } }

    public struct StringInterpolation: StringInterpolationProtocol {
        var parts: [Part] = []

        public init(literalCapacity: Int, interpolationCount: Int) {
            parts.reserveCapacity(interpolationCount * 2 + 1)
        }

        public mutating func appendLiteral(_ literal: String) {
            parts.append(Part(text: literal, isPrivate: false))
        }

        /// Any value: private. A host name, a label, a path, an error's
        /// description, anything a user or a hab wrote.
        public mutating func appendInterpolation<T>(_ value: T) {
            parts.append(Part(text: String(describing: value), isPrivate: true))
        }

        /// A count or a duration: public.
        public mutating func appendInterpolation<N: BinaryInteger>(_ n: N) {
            parts.append(Part(text: String(n), isPrivate: false))
        }

        /// A yes or no: public.
        public mutating func appendInterpolation(_ b: Bool) {
            parts.append(Part(text: b ? "true" : "false", isPrivate: false))
        }

        /// An enum word: public.
        public mutating func appendInterpolation<E: RawRepresentable>(word e: E) where E.RawValue == String {
            parts.append(Part(text: e.rawValue, isPrivate: false))
        }

        /// A fixed word the app chose (a case name, an API method): public. The
        /// source check lists the expressions that may pass here.
        public mutating func appendInterpolation(word: String) {
            parts.append(Part(text: word, isPrivate: false))
        }

        /// A duration in seconds or milliseconds: public.
        public mutating func appendInterpolation(_ d: Double) {
            parts.append(Part(text: String(d), isPrivate: false))
        }

        /// An id Flightdeck minted (pane, session, client, trace): public. The
        /// source check lists the expressions that may pass here.
        public mutating func appendInterpolation(id: String) {
            parts.append(Part(text: id, isPrivate: false))
        }

        /// An error's kind: its type and case, or an NSError's domain and code;
        /// never its description. Public.
        public mutating func appendInterpolation(kind error: Error) {
            parts.append(Part(text: AppLogMessage.kind(of: error), isPrivate: false))
        }
    }

    /// The kind of an error: `Type.case` for an enum, `domain code` for an
    /// NSError, else the type's name. No associated value and no description.
    public static func kind(of error: Error) -> String {
        let type = String(describing: Swift.type(of: error))
        let m = Mirror(reflecting: error)
        if m.displayStyle == .enum {
            if let label = m.children.first?.label {
                return type + "." + label
            }
            return type + "." + String(describing: error)
        }
        if m.displayStyle == .class || type == "NSError" || type.hasPrefix("__") {
            let ns = error as NSError
            return "\(ns.domain) \(ns.code)"
        }
        return type
    }
}

/// `diagnostics/app.log` under the app's Application Support folder: JSON
/// lines of `notice` and above, redacted, capped at 1 MiB with one old file
/// (diagnostics.md 4.10). The unified log on iOS lets an app read only its
/// current run; this file keeps a previous run's errors.
public final class AppLogFile: @unchecked Sendable {
    /// The file the app writes. Nil until the app opens it at launch, so tests
    /// and extensions write nothing.
    public static var shared: AppLogFile? {
        sharedLock.lock()
        defer { sharedLock.unlock() }
        return opened
    }

    private static var opened: AppLogFile?
    private static let sharedLock = NSLock()

    public static let defaultMax = 1 << 20

    public let url: URL
    let max: Int
    private let queue = DispatchQueue(label: "com.flightdeck.app.applog")
    private var handle: FileHandle?
    private var size = 0

    public init(url: URL, max: Int = AppLogFile.defaultMax) {
        self.url = url
        self.max = max
    }

    /// Opens the app's file at its default place and makes it the shared one.
    @discardableResult
    public static func open() -> AppLogFile? {
        guard let dir = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first else {
            return nil
        }
        let f = AppLogFile(url: dir.appendingPathComponent("Flightdeck/diagnostics/app.log"))
        sharedLock.lock()
        opened = f
        sharedLock.unlock()
        return f
    }

    /// The old file: `app.log.1`.
    public var oldURL: URL { url.appendingPathExtension("1") }

    func write(_ level: AppLogLevel, category: String, message: String, at t: Date = Date()) {
        let line = AppLogLine(t: Self.timeFormat.string(from: t), lvl: level.word, cat: category, msg: message)
        queue.async { self.append(line) }
    }

    /// Waits until every queued line is written. Tests call it.
    public func flush() { queue.sync {} }

    private static let timeFormat: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()

    private func append(_ line: AppLogLine) {
        guard var data = try? JSONEncoder.sorted.encode(line) else { return }
        data.append(0x0a)
        if handle == nil { openHandle() }
        if handle != nil, size > 0, size + data.count > max {
            rotate()
        }
        guard let h = handle else { return } // a full disk or no folder: the line is lost
        do {
            try h.write(contentsOf: data)
            size += data.count
        } catch {
            try? h.close()
            handle = nil
        }
    }

    private func openHandle() {
        let fm = FileManager.default
        let dir = url.deletingLastPathComponent()
        try? fm.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        if !fm.fileExists(atPath: url.path) {
            fm.createFile(atPath: url.path, contents: nil, attributes: [.posixPermissions: 0o600])
        }
        guard let h = try? FileHandle(forWritingTo: url) else { return }
        size = Int((try? h.seekToEnd()) ?? 0)
        handle = h
    }

    private func rotate() {
        try? handle?.close()
        handle = nil
        let fm = FileManager.default
        try? fm.removeItem(at: oldURL)
        try? fm.moveItem(at: url, to: oldURL)
        size = 0
        openHandle()
    }
}

struct AppLogLine: Codable, Equatable {
    let t: String
    let lvl: String
    let cat: String
    let msg: String
}

private extension JSONEncoder {
    static let sorted: JSONEncoder = {
        let e = JSONEncoder()
        e.outputFormatting = [.sortedKeys, .withoutEscapingSlashes]
        return e
    }()
}
