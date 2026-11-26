import Foundation

/// Runs async writes one at a time, in the order they were sent (app.md 3.19): Spotlight's
/// session index uses it, so a delete never overtakes the update before it.
public final class OrderedWrites<Write: Sendable>: Sendable {
    private let continuation: AsyncStream<Write>.Continuation
    private let consumer: Task<Void, Never>

    /// `perform` runs each write to its end before the next one starts.
    public init(priority: TaskPriority = .utility, perform: @escaping @Sendable (Write) async -> Void) {
        let (stream, continuation) = AsyncStream.makeStream(of: Write.self)
        self.continuation = continuation
        consumer = Task.detached(priority: priority) {
            for await w in stream { await perform(w) }
        }
    }

    public func send(_ write: Write) {
        continuation.yield(write)
    }

    /// Takes no more writes and returns once every write sent before has run.
    public func drain() async {
        continuation.finish()
        await consumer.value
    }
}

/// Coalesces App Shortcut parameter updates (app.md 3.19). The phrases name sessions by their
/// display name, so only a change to the set of ids and names counts; a burst of changes
/// becomes one update after `delay`.
@MainActor
public final class ShortcutParameters {
    private let delay: Duration
    private let refresh: @MainActor () -> Void
    private var applied: [String]?
    private var wanted: [String]?
    private var pending: Task<Void, Never>?

    public init(delay: Duration = .seconds(2), refresh: @escaping @MainActor () -> Void) {
        self.delay = delay
        self.refresh = refresh
    }

    /// The session list changed; updates the parameters after the delay when ids or names
    /// differ from the last update.
    public func update(_ entries: [SessionEntry]) {
        wanted = Self.signature(entries)
        pending?.cancel()
        pending = Task { [delay] in
            try? await Task.sleep(for: delay)
            guard !Task.isCancelled else { return }
            self.flush()
        }
    }

    /// Runs a pending update now: at launch, and in tests.
    public func flush() {
        pending?.cancel()
        pending = nil
        guard let wanted, wanted != applied else { return }
        applied = wanted
        refresh()
    }

    static func signature(_ entries: [SessionEntry]) -> [String] {
        entries.map { "\($0.id)\n\($0.name)" }.sorted()
    }
}
