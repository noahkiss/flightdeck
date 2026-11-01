import FlightdeckShared
import Foundation
import HabAPI
import os

/// Builds the app's fleet: demo habs with `-FlightdeckDemo YES`, else the
/// habs from the config file (the Mac), else none.
public enum AppFleet {
    @MainActor
    public static func make(defaults: UserDefaults = .standard, environment env: [String: String] = ProcessInfo.processInfo.environment) -> Fleet {
        let fleet = Fleet()
        let log = Logger(subsystem: "com.flightdeck.app", category: "config")
        let demo = DemoFleet.isRequested(defaults)
        fleet.usage = usageCenter(demo: demo)
        if demo {
            log.info("demo mode")
            Task { await DemoFleet.populate(fleet) }
            return fleet
        }
        let config: HabListConfig
        do {
            config = try HabListConfig.load(environment: env)
        } catch {
            log.error("hab config unreadable, using the local hab only: \(String(describing: error), privacy: .public)")
            config = .localOnly
        }
        for (i, h) in config.habs.enumerated() {
            do {
                let t = try h.transport(environment: env)
                fleet.add(HabConnection(id: h.id(index: i), transport: t, clientLabel: clientLabel, clientVersion: version), label: h.label)
            } catch {
                log.notice("hab \(i + 1) skipped: \(String(describing: error), privacy: .public)")
            }
        }
        addSSH(to: fleet, environment: env)
        pruneUsage(fleet)
        addPush(to: fleet, environment: env)
        return fleet
    }

    /// Usage readings and alerts. Demo mode posts no notification and keeps
    /// its readings and alert marks in a defaults suite of its own, cleared
    /// at each start, so the widget and the real alert state never see them.
    @MainActor
    static func usageCenter(demo: Bool) -> UsageCenter {
        guard demo, let d = UserDefaults(suiteName: demoUsageSuite) else {
            return UsageCenter(notifier: SystemUsageNotifier())
        }
        d.removePersistentDomain(forName: demoUsageSuite)
        let c = UsageCenter(notifier: NoopUsageNotifier(), defaults: d)
        c.reloadWidgets = {}
        return c
    }

    static let demoUsageSuite = "flightdeck.demo.usage"

    /// Drops readings seeded from the snapshot whose hab is no longer in the
    /// app (a config edit or a removed SSH entry between launches).
    @MainActor
    static func pruneUsage(_ fleet: Fleet) {
        fleet.usage?.book.retain(habIDs: Set(fleet.connections.map(\.id)))
    }

    /// The in-app SSH carrier's habs. iPhone and iPad make the device key on
    /// first start. The Mac keeps the system ssh as its default; it loads its
    /// key only when an entry needs it.
    @MainActor
    static func addSSH(to fleet: Fleet, environment env: [String: String]) {
        let listURL = SSHHabList.path(environment: env)
        let ssh = SSHSetup(keys: DeviceKeys(store: KeychainStore()), listURL: listURL, fleet: fleet, clientLabel: clientLabel, clientVersion: version)
        fleet.ssh = ssh
        #if os(iOS)
        ssh.loadKey()
        #endif
        ssh.connectAll()
    }

    /// Push registration, the approval center and the notification actions
    /// (app.md 7, 8). The actions are installed at launch, so iOS finds the
    /// delegate when an action wakes the app in the background.
    @MainActor
    static func addPush(to fleet: Fleet, environment env: [String: String]) {
        let book = HabBook(url: HabBook.path(environment: env))
        fleet.book = book
        fleet.push = PushRegistrar()
        let keys = fleet.ssh?.keys
        let listURL = SSHHabList.path(environment: env)
        let answerer = ApprovalAnswerer(
            live: { [weak fleet] host in
                guard let fleet, let e = book.entry(host: host) else { return nil }
                return fleet.connections.first { $0.id == e.connection }
            },
            oneShot: { host in
                try oneShotTransport(host: host, book: book, keys: keys, listURL: listURL, environment: env)
            },
            clientLabel: clientLabel
        )
        let center = ApprovalCenter(board: fleet.board, answerer: answerer)
        center.post = { title, body, meta in await HabActions.post(title, body, meta: meta) }
        center.withdraw = { ids in await HabActions.withdraw(requests: ids) }
        fleet.approvals = center
        let actions = HabActions(keys: keys, listURL: listURL)
        actions.approvals = center
        actions.usage = fleet.usage?.book
        actions.install()
        fleet.keepAlive.append(actions)
        #if os(iOS)
        let tracker = LiveActivityTracker(board: fleet.board, push: fleet.push!, caller: answerer, host: SystemActivities())
        fleet.activities = tracker
        ActivityAnswerBridge.handler = { hab, pane, request, allow in
            await activityAnswer(center: center, tracker: tracker, hab: hab, pane: pane, request: request, allow: allow)
        }
        #endif
    }

    /// Allow or Deny from the Live Activity: answer, then drop the buttons.
    @MainActor
    static func activityAnswer(center: ApprovalCenter, tracker: LiveActivityTracker, hab: String, pane: String, request: String, allow: Bool) async {
        guard let o = await center.answerFromActivity(host: hab, pane: pane, request: request, allow: allow) else { return }
        await tracker.answered(pane: pane, request: request, outcome: o)
    }

    /// A transport for one call to the hab a push named, from the hab book.
    static func oneShotTransport(host: String, book: HabBook, keys: DeviceKeys?, listURL: URL?, environment env: [String: String]) throws -> Transport {
        guard let e = book.entry(host: host) else {
            throw TransportError("\(host) is not a hab this app has connected to")
        }
        if e.connection.hasPrefix("nio:") {
            let id = String(e.connection.dropFirst(4))
            guard let keys, let entry = try SSHHabList.load(from: listURL).habs.first(where: { $0.id == id }) else {
                throw TransportError("that hab is no longer in the app")
            }
            if (try? keys.key(entry.keyID))?.requiresBiometry == true {
                // No Face ID prompt in the background: fail fast, never hang.
                throw TransportError("the SSH key needs Face ID: open Flightdeck to answer")
            }
            return try entry.transport(keys: keys)
        }
        let config = (try? HabListConfig.load(environment: env)) ?? .localOnly
        for (i, h) in config.habs.enumerated() where h.id(index: i) == e.connection {
            return try h.transport(environment: env)
        }
        throw TransportError("that hab is no longer in the app")
    }

    static var clientLabel: String {
        #if os(macOS)
        return "mac"
        #else
        return "phone"
        #endif
    }

    static var version: String {
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "0"
    }
}
