//
//  VPNStartController.swift
//  rootshell
//
//  Shared VPN start helper used by the app, widgets, and App Intents.
//

import Foundation
import NetworkExtension
import WidgetKit

enum VPNStartController {
    enum StartError: LocalizedError {
        case profileNotFound
        case profileNotStartable
        case hostKeyNotTrusted(host: String)
        case disconnectTimeout
        case tailnetSignInRequired

        var errorDescription: String? {
            switch self {
            case .profileNotFound:
                return String(localized: "VPN profile not found.", comment: "VPN start error: the selected profile could not be located")
            case .profileNotStartable:
                return String(localized: "VPN profile requires a saved password or SSH key before it can start in the background.", comment: "VPN start error: profile has no background-usable credential")
            case .hostKeyNotTrusted(let host):
                return String(localized: "No trusted SSH host key for \(host). Connect to this server in a regular SSH terminal session first so its host key can be verified and saved, then start the VPN.", comment: "VPN start error: the server's host key has not been accepted yet")
            case .disconnectTimeout:
                return String(localized: "Timed out waiting for the current VPN tunnel to disconnect.", comment: "VPN start error: previous tunnel did not stop in time")
            case .tailnetSignInRequired:
                return String(localized: "Tailscale needs you to sign in. Open rootshell and connect Tailscale from VPN settings.", comment: "VPN start error: Tailscale was started from a widget or Shortcut while signed out")
            }
        }
    }

    enum StartResult {
        case alreadyActive
        case started
    }

    private static let providerBundleIdentifier = "com.kk2.rootshell.vpntunnel"
    private static let providerServerAddress = "rootshell VPN"
    private static let widgetKind = "VPNControlWidget"

    /// Starts the serverless "Local Capture" tunnel.
    static func startDirect(dnsServers: [String]) async throws -> StartResult {
        VPNDirectProfile.store(VPNDirectProfile.snapshot(dnsServers: dnsServers))
        return try await start(profileID: VPNDirectProfile.id)
    }

#if !CHINA_BUILD
    /// Starts the Tailscale tunnel, with SSH egress if one is configured.
    /// `restart` reconnects a running Tailscale tunnel so new settings apply.
    static func startTailnet(restart: Bool = false) async throws -> StartResult {
        try await start(profileID: VPNTailnetProfile.id, restart: restart)
    }
#endif

    /// `background`: started from a widget, Control Center or Shortcuts, which
    /// can't show Tailscale's login page.
    static func start(profileID: UUID, restart: Bool = false, background: Bool = false) async throws -> StartResult {
        guard let snapshot = VPNSharedProfileStore.profile(id: profileID) else {
            throw StartError.profileNotFound
        }
        guard snapshot.isBackgroundStartable else {
            throw StartError.profileNotStartable
        }
        #if FLIGHTDECK
        // Widgets and Shortcuts start tunnels here, not through VPNManager.
        if snapshot.transportType == .tailscale || snapshot.transportType == .direct {
            throw StartError.profileNotStartable
        }
        #endif
        if background, snapshot.transportType == .tailscale, !VPNTailnetProfile.isSignedIn {
            throw StartError.tailnetSignInRequired
        }
        // Tailscale itself needs no host key; its SSH egress host does.
        if snapshot.transportType == .tailscale {
            if VPNTailnetProfile.settings().sshEgressProfileID != nil {
                guard let egress = VPNTailnetProfile.egressSnapshot() else {
                    throw StartError.profileNotFound
                }
                try requireTrustedHostKeys(egress)
            }
        } else if snapshot.transportType != .direct {
            try requireTrustedHostKeys(snapshot)
        }

        let manager = try await getOrCreateManager()
        let currentStatus = manager.connection.status
        let currentProfileID = activeProfileID(from: manager)

        let requestedRelay = snapshot.transportType == .tssh && snapshot.jumpHost?.tsshRelay != nil
        let activeRelay = ((manager.protocolConfiguration as? NETunnelProviderProtocol)?
            .providerConfiguration?["tsshRelay"] as? Bool) ?? false
        if !restart, currentProfileID == snapshot.id, activeRelay == requestedRelay,
           (currentStatus == .connecting || currentStatus == .connected || currentStatus == .reasserting) {
            writeWidgetState(for: snapshot, status: currentStatus)
            reloadWidgetTimelines()
            return .alreadyActive
        }

        if currentStatus == .disconnecting || currentStatus == .connecting || currentStatus == .connected || currentStatus == .reasserting {
            manager.connection.stopVPNTunnel()
            let didDisconnect = await waitForDisconnect(manager)
            guard didDisconnect else {
                throw StartError.disconnectTimeout
            }
            try await manager.loadFromPreferences()
        }

        if try await applyConfiguration(to: manager, snapshot: snapshot) {
            try await manager.saveToPreferences()
            try await manager.loadFromPreferences()
        }

        writeWidgetState(for: snapshot, statusOverride: "connecting")
        reloadWidgetTimelines()

        do {
            try manager.connection.startVPNTunnel(options: [
                "profileID": snapshot.id.uuidString as NSString,
                "transportType": snapshot.transportType.rawValue as NSString,
            ])
        } catch {
            writeDisconnectedWidgetState()
            reloadWidgetTimelines()
            throw error
        }

        return .started
    }

    /// Stops a background Tailscale start that turned out to need a login.
    static func stopForSignIn() async {
        guard let manager = try? await NETunnelProviderManager.loadAllFromPreferences().first else { return }
        manager.connection.stopVPNTunnel()
        _ = await VPNConnectionPoller.pollForDisconnection(manager: manager, checkSharedState: false)
    }

    /// The VPN never prompts for host keys: require either a key accepted
    /// in a regular SSH session or a trusted host CA covering the host
    /// (both mirrored into the snapshot) before starting.
    private static func requireTrustedHostKeys(_ snapshot: VPNSharedProfileSnapshot) throws {
        guard snapshot.isBackgroundStartable else {
            throw StartError.profileNotStartable
        }
        guard snapshot.hostKey != nil || !(snapshot.trustedCAKeys ?? []).isEmpty else {
            throw StartError.hostKeyNotTrusted(host: snapshot.host)
        }
        if let jump = snapshot.jumpChain.first(where: { $0.hostKey == nil && ($0.trustedCAKeys ?? []).isEmpty }) {
            throw StartError.hostKeyNotTrusted(host: jump.host)
        }
    }

    private static func getOrCreateManager() async throws -> NETunnelProviderManager {
        let managers = try await NETunnelProviderManager.loadAllFromPreferences()
        if let existing = managers.first {
            return existing
        }
        return NETunnelProviderManager()
    }

    private static func activeProfileID(from manager: NETunnelProviderManager) -> UUID? {
        if let widgetProfileID = VPNWidgetState.read()?.profileID,
           manager.connection.status == .connecting || manager.connection.status == .connected || manager.connection.status == .reasserting {
            return widgetProfileID
        }

        guard let proto = manager.protocolConfiguration as? NETunnelProviderProtocol,
              let providerConfiguration = proto.providerConfiguration,
              let rawProfileID = providerConfiguration["profileID"] as? String else {
            return nil
        }
        return UUID(uuidString: rawProfileID)
    }

    /// Apply the current profile snapshot to the manager's protocol configuration.
    /// Reuses the existing `NETunnelProviderProtocol` if present so we don't wipe
    /// any iOS-side blessed state. Returns `true` if anything actually changed and
    /// the caller should re-save preferences.
    private static func applyConfiguration(
        to manager: NETunnelProviderManager,
        snapshot: VPNSharedProfileSnapshot
    ) async throws -> Bool {
        let proto = (manager.protocolConfiguration as? NETunnelProviderProtocol) ?? NETunnelProviderProtocol()
        let desiredProviderConfig: [String: NSObject] = [
            "tsshRelay": NSNumber(value: snapshot.transportType == .tssh && snapshot.jumpHost?.tsshRelay != nil),
            "profileID": snapshot.id.uuidString as NSString,
            "transportType": snapshot.transportType.rawValue as NSString,
        ]

        var dirty = false

        if proto.providerBundleIdentifier != providerBundleIdentifier {
            proto.providerBundleIdentifier = providerBundleIdentifier
            dirty = true
        }
        if proto.serverAddress != providerServerAddress {
            proto.serverAddress = providerServerAddress
            dirty = true
        }
        if !providerConfigurationEquals(proto.providerConfiguration, desiredProviderConfig) {
            proto.providerConfiguration = desiredProviderConfig
            dirty = true
        }
        if manager.protocolConfiguration !== proto {
            manager.protocolConfiguration = proto
            dirty = true
        }
        if manager.localizedDescription != providerServerAddress {
            manager.localizedDescription = providerServerAddress
            dirty = true
        }
        if !manager.isEnabled {
            manager.isEnabled = true
            dirty = true
        }

        return dirty
    }

    private static func providerConfigurationEquals(
        _ lhs: [String: Any]?,
        _ rhs: [String: NSObject]
    ) -> Bool {
        guard let lhs, lhs.count == rhs.count else { return false }
        for (key, rhsValue) in rhs {
            guard let lhsValue = lhs[key] as? NSObject, lhsValue == rhsValue else {
                return false
            }
        }
        return true
    }

    private static func waitForDisconnect(
        _ manager: NETunnelProviderManager,
        timeout: Duration = .seconds(8)
    ) async -> Bool {
        let clock = ContinuousClock()
        let deadline = clock.now + timeout

        while true {
            let status = manager.connection.status
            if status == .disconnected || status == .invalid {
                return true
            }
            if clock.now >= deadline {
                return false
            }
            try? await Task.sleep(for: .milliseconds(150))
        }
    }

    private static func writeWidgetState(
        for snapshot: VPNSharedProfileSnapshot,
        status: NEVPNStatus? = nil,
        statusOverride: String? = nil
    ) {
        let effectiveStatus: String
        if let statusOverride {
            effectiveStatus = statusOverride
        } else {
            switch status {
            case .connected:
                effectiveStatus = "connected"
            case .connecting:
                effectiveStatus = "connecting"
            case .reasserting:
                effectiveStatus = "reconnecting"
            case .disconnecting:
                effectiveStatus = "disconnecting"
            default:
                effectiveStatus = "disconnected"
            }
        }

        VPNWidgetState.write(
            VPNWidgetState(
                status: effectiveStatus,
                profileID: snapshot.id,
                profileName: snapshot.name,
                host: snapshot.host,
                connectedSince: nil,
                lastUpdated: Date()
            )
        )
    }

    private static func writeDisconnectedWidgetState() {
        VPNWidgetState.write(
            VPNWidgetState(
                status: "disconnected",
                profileID: nil,
                profileName: nil,
                host: nil,
                connectedSince: nil,
                lastUpdated: Date()
            )
        )
    }

    private static func reloadWidgetTimelines() {
        WidgetCenter.shared.reloadTimelines(ofKind: widgetKind)
        #if !os(visionOS)
        ControlCenter.shared.reloadControls(ofKind: "VPNControlCenterToggle")
        #endif
    }
}
