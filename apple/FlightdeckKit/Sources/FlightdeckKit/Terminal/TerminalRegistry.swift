import SwiftUI

/// The terminal views that are open, so the lifecycle rule closes their data
/// connections with the control connections (app.md 12.1) and resumes them
/// by offset in the foreground. The fleet owns one; views register with it.
@MainActor
public final class TerminalRegistry {
    private struct Weak {
        weak var model: TerminalModel?
    }

    private var models: [ObjectIdentifier: Weak] = [:]

    public init() {}

    public func add(_ m: TerminalModel) {
        models[ObjectIdentifier(m)] = Weak(model: m)
    }

    public func remove(_ m: TerminalModel) {
        models[ObjectIdentifier(m)] = nil
    }

    /// Background: every open stream closes. Foreground: the streams closed
    /// for the background open again.
    public func setActive(_ active: Bool) {
        models = models.filter { $0.value.model != nil }
        for w in models.values {
            if active { w.model?.resume() } else { w.model?.suspend() }
        }
    }

    public var count: Int { models.values.filter { $0.model != nil }.count }
}

extension EnvironmentValues {
    @Entry public var terminals: TerminalRegistry? = nil
}
