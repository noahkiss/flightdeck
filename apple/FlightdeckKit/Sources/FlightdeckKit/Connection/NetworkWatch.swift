import Foundation
import Network

/// One reading of the network path: whether it can carry traffic, and over which interfaces.
public struct NetworkPathReading: Equatable, Sendable {
    public var usable: Bool
    /// The interface names the path may use, sorted ("en0", "pdp_ip0").
    public var interfaces: [String]

    public init(usable: Bool, interfaces: [String]) {
        self.usable = usable
        self.interfaces = interfaces.sorted()
    }

    init(_ path: NWPath) {
        self.init(usable: path.status == .satisfied, interfaces: path.availableInterfaces.map(\.name))
    }
}

/// Reconnect on a network change (app.md 10.2): the path monitor's readings, and the rule
/// for when a reading retries every hab at once.
public enum NetworkWatch {
    /// Whether `next` retries the habs: a path that became usable, or a usable path on other
    /// interfaces (Wi-Fi to cellular). The first reading (`last` nil) is the state at start,
    /// not a change, so it retries nothing.
    public static func retries(after last: NetworkPathReading?, _ next: NetworkPathReading) -> Bool {
        guard let last, next.usable else { return false }
        return !last.usable || last.interfaces != next.interfaces
    }

    /// The system's path readings, from `NWPathMonitor`, until the stream is dropped.
    public static func system() -> AsyncStream<NetworkPathReading> {
        let (stream, sink) = AsyncStream.makeStream(of: NetworkPathReading.self, bufferingPolicy: .bufferingNewest(4))
        let monitor = NWPathMonitor()
        monitor.pathUpdateHandler = { sink.yield(NetworkPathReading($0)) }
        sink.onTermination = { _ in monitor.cancel() }
        monitor.start(queue: DispatchQueue(label: "flightdeck.network-watch"))
        return stream
    }
}

extension Fleet {
    /// Retries every hab at once on a network change while the app is visible (app.md 10.2).
    /// Runs until `readings` ends.
    public func watchNetwork(_ readings: AsyncStream<NetworkPathReading>) {
        networkWatch?.cancel()
        networkWatch = Task { @MainActor [weak self] in
            var last: NetworkPathReading?
            for await r in readings {
                let retry = NetworkWatch.retries(after: last, r)
                last = r
                guard retry, let self, self.isActive else { continue }
                self.log.info("network change: retrying \(self.connections.count) habs")
                await self.retryAll()
            }
        }
    }

    /// Pull to refresh (app.md 10.2): retries every hab at once, then waits up to `limit` until
    /// each is online.
    public func refresh(limit: Duration = .seconds(5)) async {
        await retryAll()
        let clock = ContinuousClock()
        let end = clock.now + limit
        while clock.now < end, !Task.isCancelled, board.habs.contains(where: { $0.state.status != .online && !$0.needsUpdate }) {
            try? await Task.sleep(for: .milliseconds(100))
        }
    }
}
