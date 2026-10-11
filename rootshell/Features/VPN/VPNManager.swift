//
//  VPNManager.swift
//  rootshell
//
//  @MainActor @Observable singleton for VPN lifecycle management.
//  Follows the BackgroundTunnelManager pattern.
//

import Foundation
@preconcurrency import NetworkExtension
import os.log
import WidgetKit

/// Manages VPN tunnel lifecycle via NEPacketTunnelProvider.
@MainActor @Observable
final class VPNManager {
    private nonisolated static let logger = Logger(subsystem: "com.rootshell", category: "VPNManager")

    static let shared = VPNManager()

    // MARK: - Published State

    private(set) var status: NEVPNStatus = .disconnected
    private(set) var activeProfileID: UUID?
    private(set) var activeProfileName: String?
    private(set) var connectedSince: Date?
    private(set) var statistics: VPNStatistics?
    private(set) var latestStatusJSON: String?
    private(set) var trafficHistory: [VPNTrafficSnapshot] = []
    private(set) var eventHistory: [VPNEvent] = []
    private(set) var lastExtensionError: String?
#if !CHINA_BUILD
    /// The profile (or Tailscale) that last connected on this device, offered
    /// for a quick reconnect. Local Capture is never remembered.
    private(set) var lastVPNProfileID: UUID? = VPNLastConnected.read()
    @ObservationIgnored private var didAttemptRecovery = false
#endif
    /// True while the macOS system extension is waiting on user approval in
    /// System Settings, so the UI can prompt the user. (macOS Standalone only.)
    private(set) var extensionApprovalPending = false

    // MARK: - Private State

    private var tunnelManager: NETunnelProviderManager?
    private var statusObserver: NSObjectProtocol?
    private var statsTimer: Timer?
    private let maxEventHistory = 100
    private nonisolated static let statusReplyTimeout: TimeInterval = 2
    /// Tracks previous status to only reload widget timelines on state transitions
    private var previousStatus: NEVPNStatus = .disconnected
    private var initializationTask: Task<Void, Never>?
    private var pendingStatusManager: NETunnelProviderManager?
    private var pendingStatusFlushScheduled = false
    private var pendingObservedConnectedDuringDeferral = false

    // MARK: - Init

    private init() {
        initializationTask = Task {
            await self.loadExistingManagerBody()
        }
#if STANDALONE && targetEnvironment(macCatalyst)
        MacVPNController.shared.onApprovalRequired = { [weak self] in self?.extensionApprovalPending = true }
        MacVPNController.shared.onApprovalResolved = { [weak self] in self?.extensionApprovalPending = false }
#endif
    }

    // MARK: - Public API

    /// Start VPN for a given connection profile.
    func startVPN(for profile: ConnectionProfile) async throws {
        Self.logger.info("Starting VPN for profile: \(profile.name)")
#if !CHINA_BUILD
        // Registers the capture config hand-off before the tunnel starts.
        _ = CaptureController.shared
#endif
        // Re-mirror snapshots so the pinned host key reflects the latest
        // known-hosts state at the moment of an app-initiated start.
        ConnectionProfileManager.shared.refreshVPNSharedProfiles()
#if STANDALONE && targetEnvironment(macCatalyst)
        // Catalyst can't host a packet tunnel; drive the native host agent,
        // which forwards the resolved config to its system extension.
        try await MacVPNController.shared.activateExtension()
        try await MacVPNController.shared.start(profileID: profile.id)
#else
        _ = try await VPNStartController.start(profileID: profile.id)
#endif
        await refreshStatusFromSystem()

        activeProfileID = profile.id
        activeProfileName = profile.name
        if status == .disconnected || status == .invalid {
            status = .connecting
        }
        previousStatus = status

        addEvent(.connected(profileID: profile.id, message: profile.sshConfig.host))
#if STANDALONE && targetEnvironment(macCatalyst)
        // No NEVPNStatusDidChange in the Catalyst app (the manager lives in the
        // host), so poll the host for status transitions.
        startStatsPolling()
#endif
    }

    /// Start the serverless "Local Capture" tunnel used for HTTP capture.
    func startDirectVPN(dnsServers: [String]) async throws {
        #if FLIGHTDECK
        throw VPNError.unsupportedTransport("Local Capture is disabled in flightdeck")
        #endif
        Self.logger.info("Starting Local Capture VPN")
#if STANDALONE && targetEnvironment(macCatalyst)
        try await MacVPNController.shared.activateExtension()
        try await MacVPNController.shared.startDirect(dnsServers: dnsServers)
#else
        _ = try await VPNStartController.startDirect(dnsServers: dnsServers)
#endif
        await refreshStatusFromSystem()

        let snapshot = VPNDirectProfile.snapshot(dnsServers: dnsServers)
        activeProfileID = snapshot.id
        activeProfileName = snapshot.name
        if status == .disconnected || status == .invalid {
            status = .connecting
        }
        previousStatus = status
        addEvent(.connected(profileID: snapshot.id, message: snapshot.name))
#if STANDALONE && targetEnvironment(macCatalyst)
        startStatsPolling()
#endif
    }

#if !CHINA_BUILD
    /// Stops a running Tailscale VPN and waits for it to go down, so the
    /// in-app engine can take over the node key.
    func stopTailnetVPNAndWait() async {
        guard isVPNActive(for: VPNTailnetProfile.id) || (activeProfileID == VPNTailnetProfile.id && status == .disconnecting) else { return }
        try? await stopVPN()
        let deadline = ContinuousClock.now + .seconds(8)
        while ContinuousClock.now < deadline, status != .disconnected, status != .invalid {
            try? await Task.sleep(for: .milliseconds(200))
        }
    }

    /// Start the Tailscale VPN; `restart` reconnects so changed settings apply.
    func startTailnetVPN(restart: Bool = false) async throws {
        #if FLIGHTDECK
        throw VPNError.unsupportedTransport("Tailscale is disabled in flightdeck")
        #endif
        Self.logger.info("Starting Tailscale VPN (restart=\(restart))")
        // The in-app engine holds the same node key.
        await TailnetInAppEngine.shared.stopForVPN()
        // Mirrors the SSH egress profile with its current host key.
        ConnectionProfileManager.shared.refreshVPNSharedProfiles()
#if STANDALONE && targetEnvironment(macCatalyst)
        try await MacVPNController.shared.activateExtension()
        try await MacVPNController.shared.startTailnet(restart: restart)
#else
        _ = try await VPNStartController.startTailnet(restart: restart)
#endif
        await refreshStatusFromSystem()

        let snapshot = VPNTailnetProfile.snapshot()
        activeProfileID = snapshot.id
        activeProfileName = snapshot.name
        if status == .disconnected || status == .invalid {
            status = .connecting
        }
        previousStatus = status
        addEvent(.connected(profileID: snapshot.id, message: snapshot.name))
#if STANDALONE && targetEnvironment(macCatalyst)
        startStatsPolling()
#endif
    }
#endif

    /// Whether the tunnel is up and able to answer provider messages.
    var isTunnelUp: Bool {
        status == .connected || status == .reasserting
    }

    /// Send a raw provider message (HTTP capture). nil when no tunnel answers.
    func sendProviderMessage(_ message: Data, timeout: TimeInterval = 8) async -> Data? {
#if STANDALONE && targetEnvironment(macCatalyst)
        return await MacVPNController.shared.providerMessage(message, timeoutSeconds: Int(timeout.rounded(.up)))
#else
        guard let session = tunnelManager?.connection as? NETunnelProviderSession,
              session.status == .connected || session.status == .reasserting else { return nil }
        return try? await withTimeout(seconds: timeout) {
            await withCheckedContinuation { continuation in
                do {
                    try session.sendProviderMessage(message) { data in
                        continuation.resume(returning: data)
                    }
                } catch {
                    continuation.resume(returning: nil)
                }
            }
        }
#endif
    }

    /// Stop the active VPN tunnel.
    func stopVPN() async throws {
#if !CHINA_BUILD
        VPNAutoRecovery.clear()
#endif
#if STANDALONE && targetEnvironment(macCatalyst)
        try await MacVPNController.shared.stop()
        extensionApprovalPending = false
        status = .disconnecting
        previousStatus = status
        addEvent(.disconnected(profileID: activeProfileID ?? UUID(), reason: "User requested"))
        connectedSince = nil
        statistics = nil
        latestStatusJSON = nil
        trafficHistory = []
        // Keep polling: the host reports "disconnecting" until the provider
        // finishes tearing down, and only a poll observing "disconnected" moves
        // the UI off the Disconnecting state (applyMacStatus stops the timer
        // when it lands there).
        startStatsPolling()
#else
        guard let manager = tunnelManager else {
            return
        }

        let disconnectingProfileID = activeProfileID
        let disconnectingProfileName = activeProfileName
        let disconnectingHost = currentWidgetHost

        manager.connection.stopVPNTunnel()
        let connectionStatus = manager.connection.status
        if connectionStatus == .connected || connectionStatus == .connecting || connectionStatus == .reasserting {
            status = .disconnecting
        } else {
            status = connectionStatus
        }
        previousStatus = status

        let profileID = activeProfileID ?? UUID()
        addEvent(.disconnected(profileID: profileID, reason: "User requested"))

        connectedSince = nil
        statistics = nil
        latestStatusJSON = nil
        trafficHistory = []

        writeWidgetState(
            statusOverride: "disconnecting",
            profileIDOverride: disconnectingProfileID,
            profileNameOverride: disconnectingProfileName,
            hostOverride: disconnectingHost
        )
        reloadWidgetTimelines()
#endif
    }

    /// Check if VPN is active for a specific profile.
    func isVPNActive(for profileID: UUID) -> Bool {
        activeProfileID == profileID && (status == .connected || status == .connecting || status == .reasserting)
    }

#if STANDALONE && targetEnvironment(macCatalyst)
    /// Map the host's status string (from the control socket) to NEVPNStatus.
    private func applyMacStatus(_ statusString: String) {
        let mapped: NEVPNStatus
        switch statusString {
        case "connected": mapped = .connected
        case "connecting": mapped = .connecting
        case "reasserting": mapped = .reasserting
        case "disconnecting": mapped = .disconnecting
        case "invalid": mapped = .invalid
        default: mapped = .disconnected
        }
        // No noteTunnelEnded here: the host owns clearing auto recovery on the
        // Mac, since only it can tell extension updates and shutdowns apart.
        if status != mapped {
            status = mapped
            previousStatus = mapped
        }
        if mapped == .connected {
            rememberActiveVPN()
        }
        if mapped == .connected, connectedSince == nil {
            connectedSince = Date()
        } else if mapped == .disconnected || mapped == .invalid {
            // Not extensionApprovalPending: the tunnel stays down while approval
            // is pending, and activateExtension() owns that flag.
            connectedSince = nil
            stopStatsPolling()
        }
    }

    /// Rebuilds session state (status, profile, stats polling) from the host,
    /// which owns the NE configuration; loadAllFromPreferences() is always
    /// empty in this app. No-op when the host isn't running (the socket
    /// connect fails fast) — a dead host can't have a session.
    private func refreshMacVPNState(shouldApply: (@MainActor () -> Bool)? = nil) async {
        guard await MacVPNController.shared.isHostResponsive(),
              let response = await MacVPNController.shared.status() else { return }
        if let shouldApply, !shouldApply() { return }
        applyMacStatus(response.status)

        switch status {
        case .connected, .connecting, .reasserting:
            if let profileID = response.profileID {
                activeProfileID = profileID
                activeProfileName =
                    ConnectionProfileManager.shared.profile(for: profileID)?.name ??
                    VPNSharedProfileStore.profile(id: profileID)?.name
                if status == .connected { rememberActiveVPN() }
            }
            if let json = response.statusJSON {
                applyStatusJSON(json)
            }
            if statsTimer == nil {
                startStatsPolling()
            }
        case .disconnected, .invalid:
            activeProfileID = nil
            activeProfileName = nil
            statistics = nil
            latestStatusJSON = nil
            trafficHistory = []
        default:
            break
        }
        writeWidgetState()
        reloadWidgetTimelines()
    }
#endif

    /// Request latest stats from extension via IPC.
    func requestStatusUpdate() async {
#if STANDALONE && targetEnvironment(macCatalyst)
        if let response = await MacVPNController.shared.status() {
            applyMacStatus(response.status)
            if let json = response.statusJSON {
                applyStatusJSON(json)
            }
        }
#else
        guard !Ghostty.isAppBackgroundedAtomic,
              !Ghostty.isInResumeQuietWindowAtomic,
              !ForegroundActivationGate.shared.isUnsafeForSceneMutation else {
            LifecycleDebugLogger.shared.checkpoint("VPN.stats.skipped", ms: nil, [
                ("backgrounded", Ghostty.isAppBackgroundedAtomic),
                ("quiet", Ghostty.isInResumeQuietWindowAtomic),
                ("activationGateUnsafe", ForegroundActivationGate.shared.isUnsafeForSceneMutation),
            ])
            return
        }

        guard let manager = tunnelManager else {
            Self.logger.debug("requestStatusUpdate: no tunnelManager")
            return
        }
        guard let session = manager.connection as? NETunnelProviderSession else {
            Self.logger.debug("requestStatusUpdate: connection is not NETunnelProviderSession")
            return
        }

        // Bridge sendProviderMessage's completion handler into async/await
        // so statistics is set directly in this async context. The provider
        // can hold a reply indefinitely, so stop waiting after the timeout.
        let responseData: Data?
        do {
            responseData = try await withTimeout(seconds: Self.statusReplyTimeout) {
                await withCheckedContinuation { continuation in
                    do {
                        let request = Data("getStatus".utf8)
                        try session.sendProviderMessage(request) { data in
                            continuation.resume(returning: data)
                        }
                    } catch {
                        let errorMsg = error.localizedDescription
                        Self.logger.error("sendProviderMessage failed: \(errorMsg)")
                        continuation.resume(returning: nil)
                    }
                }
            }
        } catch {
            Self.logger.warning("requestStatusUpdate: no reply within \(Self.statusReplyTimeout)s")
            return
        }

        guard let data = responseData else {
            Self.logger.debug("requestStatusUpdate: nil response data")
            return
        }
        guard let json = String(data: data, encoding: .utf8) else {
            Self.logger.debug("requestStatusUpdate: response not valid UTF-8")
            return
        }
        applyStatusJSON(json)
#endif
    }

    /// Parse the provider's `getStatus` JSON into stats / traffic / connectedSince.
    /// Shared by the iOS path (direct `sendProviderMessage`) and the macOS path
    /// (relayed from the host over the control socket).
    private func applyStatusJSON(_ json: String) {
        if latestStatusJSON != json {
            self.latestStatusJSON = json
        }
        guard let stats = VPNStatistics.fromGoStatus(json) else {
            Self.logger.debug("applyStatusJSON: failed to parse GoStatus from: \(json)")
            return
        }
        if statistics != stats {
            self.statistics = stats
        }
        if let extensionStartDate = stats.connectedSince,
           connectedSince != extensionStartDate {
            self.connectedSince = extensionStartDate
        }

        // Parse traffic history from the raw JSON (not part of VPNStatistics).
        // Always assign to ensure @Observable fires on every poll.
        if let rawData = json.data(using: .utf8),
           let rawObj = try? JSONSerialization.jsonObject(with: rawData) as? [String: Any],
           let history = rawObj["trafficHistory"] as? [[Any]] {
            let snapshots = VPNTrafficSnapshot.fromJSONArray(history)
            if trafficHistory != snapshots {
                self.trafficHistory = snapshots
            }
        } else {
            if !trafficHistory.isEmpty {
                self.trafficHistory = []
            }
        }

        // Feed Live Activity with real-time VPN stats
        #if canImport(ActivityKit) && !targetEnvironment(macCatalyst)
        let profileName = activeProfileName ?? "VPN"
        let host = activeProfileHost
        LiveActivityManager.shared.updateVPNState(
            profileName: profileName,
            host: host,
            status: "connected",
            bytesIn: stats.bytesIn,
            bytesOut: stats.bytesOut,
            activeConnections: stats.activeConnections,
            connectedSince: connectedSince
        )
        #endif

        // Write widget state after the resume quiet window only. The poll is
        // already skipped during the window, so this does not amplify scene
        // activation.
        writeWidgetState()
    }

    /// Reconcile in-memory VPN state with NetworkExtension state.
    /// Useful after foreground transitions where status notifications may be missed.
    ///
    /// `shouldApply` is an optional pre-mutation guard called once per await
    /// resume (after `initializationTask` and after `loadAllFromPreferences`).
    /// Lifecycle callers pass a `LifecycleEpoch` check so a backgrounding that
    /// arrives while the NetworkExtension XPC is in flight aborts the
    /// `tunnelManager` / `status` / `activeProfileID` / etc. mutations
    /// instead of publishing them onto a backgrounded scene.
    func refreshStatusFromSystem(shouldApply: (@MainActor () -> Bool)? = nil) async {
        await initializationTask?.value

        if let shouldApply, !shouldApply() {
            LifecycleDebugLogger.shared.checkpoint("VPN.refresh.applySkipped", ms: nil, [
                ("reason", "guardFalseAfterInit"),
            ])
            return
        }
        reloadLastVPN()

#if STANDALONE && targetEnvironment(macCatalyst)
        await refreshMacVPNState(shouldApply: shouldApply)
#else
        do {
            let managers = try await NETunnelProviderManager.loadAllFromPreferences()

            if let shouldApply, !shouldApply() {
                LifecycleDebugLogger.shared.checkpoint("VPN.refresh.applySkipped", ms: nil, [
                    ("reason", "guardFalseAfterLoad"),
                ])
                return
            }

            guard let manager = managers.first else {
                tunnelManager = nil
                stopStatsPolling()
                status = .disconnected
                previousStatus = .disconnected
                activeProfileID = nil
                activeProfileName = nil
                connectedSince = nil
                statistics = nil
                latestStatusJSON = nil
                trafficHistory = []
                writeWidgetState()
                reloadWidgetTimelines()
                return
            }

            tunnelManager = manager
            observeStatus(manager)

            let systemStatus = manager.connection.status
            noteTunnelEnded(from: status, to: systemStatus)
            status = systemStatus
            previousStatus = systemStatus

            switch systemStatus {
            case .connected:
                restoreActiveProfile(from: manager)
                if connectedSince == nil {
                    connectedSince = manager.connection.connectedDate
                }
                startStatsPolling()
            case .connecting, .reasserting:
                restoreActiveProfile(from: manager)
            case .disconnecting:
                break
            case .disconnected, .invalid:
                stopStatsPolling()
                activeProfileID = nil
                activeProfileName = nil
                connectedSince = nil
                statistics = nil
                latestStatusJSON = nil
                trafficHistory = []
            @unknown default:
                break
            }

            writeWidgetState()
            reloadWidgetTimelines()
        } catch {
            let errorMsg = error.localizedDescription
            Self.logger.error("refreshStatusFromSystem failed: \(errorMsg)")
        }
#endif
    }

    // MARK: - Private Helpers

    private func loadExistingManagerBody() async {
#if STANDALONE && targetEnvironment(macCatalyst)
        // The NE configuration lives in the host agent, not this app, so
        // loadAllFromPreferences() finds nothing here. Ask the host instead;
        // a live tunnel must survive an app relaunch (visible + stoppable).
        await refreshMacVPNState()
#else
        do {
            let managers = try await NETunnelProviderManager.loadAllFromPreferences()
            if let manager = managers.first {
                tunnelManager = manager
                status = manager.connection.status
                observeStatus(manager)

                // Cold-start: restore active profile info from saved NE configuration
                if manager.connection.status == .connected || manager.connection.status == .connecting {
                    restoreActiveProfile(from: manager)
                    // Use NEVPNConnection.connectedDate for immediate display
                    // before the first stats poll returns the extension's timestamp.
                    connectedSince = manager.connection.connectedDate
                }

                // Cold-start: if VPN is already connected when app launches,
                // observeStatus won't fire a .connected transition, so start
                // polling immediately.
                if manager.connection.status == .connected {
                    startStatsPolling()
                }
            }
            // Always write widget state so the profile picker is populated
            // even when no VPN tunnel is active.
            writeWidgetState()
            reloadWidgetTimelines()
        } catch {
            let errorMsg = error.localizedDescription
            Self.logger.error("Failed to load VPN manager: \(errorMsg)")
        }
#endif
    }

    /// Records the active VPN as the one to reconnect to. Called only once it
    /// is connected, so a failed attempt doesn't replace a working choice. The
    /// iOS tunnel records it too; on the Mac only the app can write the group.
    private func rememberActiveVPN() {
#if !CHINA_BUILD
        guard let id = activeProfileID else { return }
        VPNLastConnected.record(id)
        VPNAutoRecovery.markRunning(id)
        reloadLastVPN()
#endif
    }

    /// A tunnel that goes down while the app watches wasn't up when it last ran.
    /// A failed connect (from `.connecting`) keeps the marker for the next launch.
    private func noteTunnelEnded(from oldStatus: NEVPNStatus, to newStatus: NEVPNStatus) {
#if !CHINA_BUILD
        guard newStatus == .disconnected || newStatus == .invalid,
              oldStatus == .connected || oldStatus == .reasserting || oldStatus == .disconnecting else { return }
        VPNAutoRecovery.clear()
#endif
    }

    /// Picks up connections made while the app wasn't running (widget,
    /// Shortcuts, Control Center), which the tunnel recorded.
    func reloadLastVPN() {
#if !CHINA_BUILD
        let stored = VPNLastConnected.read()
        if stored != lastVPNProfileID { lastVPNProfileID = stored }
#endif
    }

    /// Restore activeProfileID and activeProfileName from the saved NE protocol configuration.
    private func restoreActiveProfile(from manager: NETunnelProviderManager) {
        defer {
            if manager.connection.status == .connected { rememberActiveVPN() }
        }
        if let widgetState = VPNWidgetState.read(),
           let profileID = widgetState.profileID,
           manager.connection.status == .connecting || manager.connection.status == .connected || manager.connection.status == .reasserting {
            activeProfileID = profileID
            activeProfileName =
                widgetState.profileName ??
                ConnectionProfileManager.shared.profile(for: profileID)?.name ??
                VPNSharedProfileStore.profile(id: profileID)?.name
            return
        }

        guard let proto = manager.protocolConfiguration as? NETunnelProviderProtocol,
              let configDict = proto.providerConfiguration,
              let profileIDString = configDict["profileID"] as? String,
              let profileID = UUID(uuidString: profileIDString) else {
            return
        }

        activeProfileID = profileID
        activeProfileName =
            ConnectionProfileManager.shared.profile(for: profileID)?.name ??
            VPNSharedProfileStore.profile(id: profileID)?.name
    }

    private func observeStatus(_ manager: NETunnelProviderManager) {
        // Remove old observer
        if let observer = statusObserver {
            NotificationCenter.default.removeObserver(observer)
        }

        statusObserver = NotificationCenter.default.addObserver(
            forName: .NEVPNStatusDidChange,
            object: manager.connection,
            queue: .main
        ) { [weak self] _ in
            let observedStatus = manager.connection.status
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.handleObservedStatusChange(from: manager, observedStatus: observedStatus)
            }
        }
    }

    private func handleObservedStatusChange(from manager: NETunnelProviderManager, observedStatus: NEVPNStatus) {
        LifecycleDebugLogger.shared.checkpoint("VPN.status.received", ms: nil, [
            ("status", observedStatus.displayString),
            ("backgrounded", Ghostty.isAppBackgroundedAtomic),
            ("quiet", Ghostty.isInResumeQuietWindowAtomic),
            ("activationGateUnsafe", ForegroundActivationGate.shared.isUnsafeForSceneMutation),
        ])

        if Ghostty.isAppBackgroundedAtomic ||
            Ghostty.isInResumeQuietWindowAtomic ||
            ForegroundActivationGate.shared.isUnsafeForSceneMutation {
            pendingStatusManager = manager
            if observedStatus == .connected {
                pendingObservedConnectedDuringDeferral = true
            }
            scheduleDeferredStatusFlush()
            LifecycleDebugLogger.shared.checkpoint("VPN.status.deferred", ms: nil, [
                ("status", observedStatus.displayString),
            ])
            return
        }

        applyObservedStatus(from: manager)
    }

    private func scheduleDeferredStatusFlush() {
        guard pendingStatusManager != nil else { return }
        guard !pendingStatusFlushScheduled else { return }
        pendingStatusFlushScheduled = true
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.2) { [weak self] in
            guard let self else { return }
            self.pendingStatusFlushScheduled = false
            guard self.pendingStatusManager != nil else { return }
            guard !Ghostty.isAppBackgroundedAtomic,
                  !Ghostty.isInResumeQuietWindowAtomic,
                  !ForegroundActivationGate.shared.isUnsafeForSceneMutation else {
                self.scheduleDeferredStatusFlush()
                return
            }
            guard let manager = self.pendingStatusManager else { return }
            self.pendingStatusManager = nil
            self.applyObservedStatus(from: manager)
        }
    }

    private func applyObservedStatus(from manager: NETunnelProviderManager) {
        let newStatus = manager.connection.status
        let oldProfileID = activeProfileID
        let oldProfileName = activeProfileName
        let observedConnectedDuringDeferral = pendingObservedConnectedDuringDeferral
        pendingObservedConnectedDuringDeferral = false

        if status != newStatus {
            status = newStatus
        }

        let statusChanged = newStatus != previousStatus
        noteTunnelEnded(from: previousStatus, to: newStatus)
        previousStatus = newStatus

        LifecycleDebugLogger.shared.checkpoint("VPN.status.applied", ms: nil, [
            ("status", newStatus.displayString),
            ("changed", statusChanged),
            ("sawConnected", observedConnectedDuringDeferral),
        ])

        switch newStatus {
        case .connected:
            restoreActiveProfile(from: manager)
            if let connectedDate = manager.connection.connectedDate,
               connectedSince == nil {
                connectedSince = connectedDate
            }
            if statusChanged || statsTimer == nil {
                startStatsPolling()
            }
            if statusChanged || activeProfileID != oldProfileID || activeProfileName != oldProfileName {
                writeWidgetState()
                reloadWidgetTimelines()
                #if canImport(ActivityKit) && !targetEnvironment(macCatalyst)
                LiveActivityManager.shared.syncVPNStateFromWidgetState(reason: "status connected")
                if NetworkInfoLiveActivityBridge.shared.isRunning {
                    NetworkInfoLiveActivityBridge.shared.resolveAfterForegroundQuietWindow()
                }
                #endif
            }
        case .disconnecting:
            if statusChanged {
                writeWidgetState()
                reloadWidgetTimelines()
                #if canImport(ActivityKit) && !targetEnvironment(macCatalyst)
                LiveActivityManager.shared.syncVPNStateFromWidgetState(reason: "status disconnecting")
                #endif
            }
        case .disconnected, .invalid:
            if connectedSince != nil { connectedSince = nil }
            if statistics != nil { statistics = nil }
            if !trafficHistory.isEmpty { trafficHistory = [] }
            stopStatsPolling()
            if let extError = Self.readExtensionError() {
                lastExtensionError = extError
                Self.logger.error("Extension error: \(extError)")
            }
            if let profileID = activeProfileID {
                let reason = lastExtensionError ?? "Extension stopped"
                activeProfileID = nil
                activeProfileName = nil
                addEvent(.disconnected(profileID: profileID, reason: reason))
            }
            #if canImport(ActivityKit) && !targetEnvironment(macCatalyst)
            if statusChanged || oldProfileID != nil {
                LiveActivityManager.shared.clearVPNState()
                if NetworkInfoLiveActivityBridge.shared.isRunning {
                    NetworkInfoLiveActivityBridge.shared.resolveAfterForegroundQuietWindow()
                }
            }
            #endif
            if statusChanged || oldProfileID != nil || oldProfileName != nil {
                writeWidgetState()
                reloadWidgetTimelines()
            }
        case .connecting, .reasserting:
            if observedConnectedDuringDeferral {
                restoreActiveProfile(from: manager)
                if connectedSince == nil {
                    connectedSince = manager.connection.connectedDate ?? VPNWidgetState.read()?.connectedSince
                }
                if statsTimer == nil {
                    startStatsPolling()
                }
            }
            if statusChanged || observedConnectedDuringDeferral {
                writeWidgetState()
                reloadWidgetTimelines()
                #if canImport(ActivityKit) && !targetEnvironment(macCatalyst)
                // syncVPNStateFromWidgetState reads the app-group file we
                // just wrote; keep this ordering so deferred
                // connecting/reasserting states are reflected exactly.
                LiveActivityManager.shared.syncVPNStateFromWidgetState(reason: "status \(newStatus.logDescription)")
                if NetworkInfoLiveActivityBridge.shared.isRunning {
                    NetworkInfoLiveActivityBridge.shared.resolveAfterForegroundQuietWindow()
                }
                #endif
            }
        default:
            break
        }
    }

    private func startStatsPolling() {
        stopStatsPolling()
        // Fire first poll immediately so stats appear without waiting for the timer interval.
        Task { await requestStatusUpdate() }
        statsTimer = Timer.scheduledTimer(withTimeInterval: 2.0, repeats: true) { [weak self] _ in
            Task { @MainActor in
                await self?.requestStatusUpdate()
            }
        }
    }

    private func stopStatsPolling() {
        statsTimer?.invalidate()
        statsTimer = nil
    }

    // MARK: - Shared Status Helpers

    /// The host of the active profile, read from the NE protocol configuration.
    private var activeProfileHost: String? {
        guard let proto = tunnelManager?.protocolConfiguration as? NETunnelProviderProtocol else {
            return nil
        }
        return proto.serverAddress
    }

    private var currentWidgetHost: String? {
        let host = activeProfileHost
        if host == nil || host == "rootshell VPN" {
            return VPNWidgetState.read()?.host
        }
        return host
    }

    private func writeWidgetState(
        statusOverride: String? = nil,
        profileIDOverride: UUID? = nil,
        profileNameOverride: String? = nil,
        hostOverride: String? = nil
    ) {
        let vpnStatus: String
        if let override = statusOverride {
            vpnStatus = override
        } else {
            switch status {
            case .connected: vpnStatus = "connected"
            case .connecting: vpnStatus = "connecting"
            case .reasserting: vpnStatus = "reconnecting"
            case .disconnecting: vpnStatus = "disconnecting"
            default: vpnStatus = "disconnected"
            }
        }

        let existingState = VPNWidgetState.read()
        let resolvedProfileID = profileIDOverride ?? activeProfileID ?? existingState?.profileID
        let resolvedProfileName = profileNameOverride ?? activeProfileName ?? existingState?.profileName
        let resolvedHost = hostOverride ?? currentWidgetHost ?? existingState?.host
        let resolvedConnectedSince: Date? =
            vpnStatus == "connected"
            ? (connectedSince ?? existingState?.connectedSince)
            : nil
        let resolvedStatus: String
        if let existingState,
           existingState.profileID == resolvedProfileID,
           existingState.status == "connected",
           (vpnStatus == "connecting" || vpnStatus == "reconnecting") {
            resolvedStatus = "connected"
        } else {
            resolvedStatus = vpnStatus
        }

        let state = VPNWidgetState(
            status: resolvedStatus,
            profileID: resolvedProfileID,
            profileName: resolvedProfileName,
            host: resolvedHost,
            connectedSince: resolvedConnectedSince,
            lastUpdated: Date()
        )
        VPNWidgetState.write(state)
    }

    private func reloadWidgetTimelines() {
        LifecycleDebugLogger.shared.checkpoint("Widget.reload", ms: nil, [
            ("kind", "VPNControlWidget"),
        ])
        WidgetCenter.shared.reloadTimelines(ofKind: "VPNControlWidget")
        #if !os(visionOS)
        ControlCenter.shared.reloadControls(ofKind: "VPNControlCenterToggle")
        #endif
    }

    // MARK: - Widget Connect Request

    /// Called from onOpenURL when the widget opens the app via rootshell://vpn/connect/<profileID>
    /// Returns true when the request is already active or a new tunnel start is issued.
    func handleWidgetConnectRequest(profileID: UUID) async -> Bool {
        await initializationTask?.value
        await refreshStatusFromSystem()
        do {
            #if !CHINA_BUILD && os(iOS) && !targetEnvironment(macCatalyst)
            if profileID == VPNTailnetProfile.id {
                // The widget sends Tailscale here when it needs a login.
                try await startTailnetVPN()
                TailnetLoginCoordinator.shared.watch()
                return true
            }
            #endif
            _ = try await VPNStartController.start(profileID: profileID)
            await refreshStatusFromSystem()
            return isVPNActive(for: profileID) || (activeProfileID == profileID && status == .connecting)
        } catch {
            Self.logger.error("Widget connect request failed: \(error.localizedDescription)")
            await refreshStatusFromSystem()
            return false
        }
    }

#if !CHINA_BUILD && (!targetEnvironment(macCatalyst) || STANDALONE)
    /// Reconnects the VPN that was up when the app last ran, e.g. after a
    /// reboot or app update. Runs at most once per launch.
    func recoverLastVPNIfNeeded() async {
        guard !didAttemptRecovery else { return }
        didAttemptRecovery = true
        guard SettingsStore.shared.value(Settings.VPN.autoRecovery) else { return }
        await initializationTask?.value
        // Read after the await: a stop during startup clears the marker.
        guard let id = VPNAutoRecovery.pendingProfileID(),
              !status.isActive, status != .disconnecting else { return }
        guard !Self.isOtherVPNActive() else {
            Self.logger.info("Auto recovery skipped: another VPN is connected")
            return
        }
        Self.logger.info("Auto recovery: reconnecting \(id.uuidString, privacy: .public)")
        do {
            if id == VPNTailnetProfile.id {
                #if os(iOS) && (!targetEnvironment(macCatalyst) || STANDALONE)
                // rootshell Only runs Tailscale in-app instead.
                guard TailnetModeSettings.load().effectiveMode == .wholeDevice else { return }
                try await startTailnetVPN()
                TailnetLoginCoordinator.shared.watch()
                #endif
            } else if let profile = ConnectionProfileManager.shared.profiles.first(where: { $0.id == id && $0.isVPNCapable }) {
                try await startVPN(for: profile)
            }
        } catch {
            Self.logger.error("Auto recovery failed: \(error.localizedDescription)")
        }
    }

    /// Any tunnel interface with scoped network settings. Only meaningful while
    /// ours is down, when such an interface belongs to another VPN app.
    private static func isOtherVPNActive() -> Bool {
        guard let settings = CFNetworkCopySystemProxySettings()?.takeRetainedValue() as? [String: Any],
              let scoped = settings["__SCOPED__"] as? [String: Any] else { return false }
        let prefixes = ["utun", "ipsec", "ppp", "tun", "tap"]
        return scoped.keys.contains { name in prefixes.contains { name.hasPrefix($0) } }
    }
#endif

    private func addEvent(_ event: VPNEvent) {
        eventHistory.append(event)
        if eventHistory.count > maxEventHistory {
            eventHistory.removeFirst(eventHistory.count - maxEventHistory)
        }
    }

    /// Read the last error written by the VPN extension via app group.
    private static func readExtensionError() -> String? {
        guard let containerURL = FileManager.default.containerURL(
            forSecurityApplicationGroupIdentifier: AppIdentifiers.defaultAppGroupID
        ) else { return nil }

        let fileURL = containerURL.appendingPathComponent("vpn_last_error.txt")
        guard let data = try? Data(contentsOf: fileURL) else { return nil }
        // Clean up after reading
        try? FileManager.default.removeItem(at: fileURL)
        return String(data: data, encoding: .utf8)
    }
}

// MARK: - NEVPNStatus Extension

extension NEVPNStatus {
    var logDescription: String {
        switch self {
        case .invalid: return "invalid"
        case .disconnected: return "disconnected"
        case .connecting: return "connecting"
        case .connected: return "connected"
        case .reasserting: return "reasserting"
        case .disconnecting: return "disconnecting"
        @unknown default: return "unknown"
        }
    }

    var displayString: String {
        switch self {
        case .invalid: return String(localized: "Invalid", comment: "VPN status: invalid configuration")
        case .disconnected: return String(localized: "Disconnected", comment: "VPN status: not connected")
        case .connecting: return String(localized: "Connecting", comment: "VPN status: establishing connection")
        case .connected: return String(localized: "Connected", comment: "VPN status: active connection")
        case .reasserting: return String(localized: "Reconnecting", comment: "VPN status: re-establishing connection")
        case .disconnecting: return String(localized: "Disconnecting", comment: "VPN status: tearing down connection")
        @unknown default: return String(localized: "Unknown", comment: "VPN status: unknown state")
        }
    }

    var isActive: Bool {
        switch self {
        case .connecting, .connected, .reasserting:
            return true
        default:
            return false
        }
    }
}
