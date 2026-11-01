import Foundation
import os

/// Builds the app's fleet: demo habs with `-FlightdeckDemo YES`, else the
/// habs from the config file (the Mac), else none.
public enum AppFleet {
    @MainActor
    public static func make(defaults: UserDefaults = .standard, environment env: [String: String] = ProcessInfo.processInfo.environment) -> Fleet {
        let fleet = Fleet()
        let log = Logger(subsystem: "com.flightdeck.app", category: "config")
        if DemoFleet.isRequested(defaults) {
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
        return fleet
    }

    /// The in-app SSH carrier's habs. iPhone and iPad make the device key on
    /// first start and install the notification actions. The Mac keeps the
    /// system ssh as its default; it loads its key only when an entry needs it.
    @MainActor
    static func addSSH(to fleet: Fleet, environment env: [String: String]) {
        let listURL = SSHHabList.path(environment: env)
        let ssh = SSHSetup(keys: DeviceKeys(store: KeychainStore()), listURL: listURL, fleet: fleet, clientLabel: clientLabel, clientVersion: version)
        fleet.ssh = ssh
        #if os(iOS)
        ssh.loadKey()
        let actions = HabActions(keys: ssh.keys, listURL: listURL)
        actions.install()
        fleet.keepAlive.append(actions)
        #endif
        ssh.connectAll()
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
