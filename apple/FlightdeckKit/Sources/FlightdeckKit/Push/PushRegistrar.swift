import Foundation
import HabAPI
import Observation
import os

/// Registers this device's APNs token with every connected hab (app.md 8.1,
/// 17): on each new connection, and on every hab again when the token or a
/// setting changes. A hab that does not advertise `push` in its welcome gets
/// no call; Settings says push is not set up there (app.md 8.4).
///
/// The app disconnects in the background (app.md 12.1), so "every connected
/// hab" is every hab each time the app comes back: the registration is
/// idempotent on the hab (one token per device id).
@MainActor
@Observable
public final class PushRegistrar {
    /// Push as one hab sees this device.
    public enum HabPush: Equatable, Sendable {
        /// Online with push, no token yet (permission not granted, or APNs has
        /// not answered).
        case waiting
        /// The hab's welcome has no `push`: live updates only.
        case notSetUp
        case registering
        case registered
        /// The call failed; the next connection or token change tries again.
        case failed(String)
    }

    /// Whether the system lets Flightdeck show notifications.
    public enum Permission: Equatable, Sendable {
        case unknown
        case granted
        case denied
    }

    /// The APNs device token, lowercase hex; nil until APNs gives one.
    public private(set) var token: String?
    /// By hab connection id.
    public private(set) var status: [String: HabPush] = [:]
    public private(set) var permission: Permission = .unknown
    /// Why APNs gave no token, when it said.
    public private(set) var tokenError: String?

    public nonisolated let deviceID: String
    public nonisolated let environment: PushEnvironment

    /// "Show commands in notifications" (app.md 8.3); on by default.
    public var showCommands: Bool {
        didSet {
            guard showCommands != oldValue else { return }
            defaults.set(!showCommands, forKey: Keys.hideCommands)
            registerAll()
        }
    }

    /// A passive banner when a turn finishes (app.md 18.5); off by default.
    public var doneBanners: Bool {
        didSet {
            guard doneBanners != oldValue else { return }
            defaults.set(doneBanners, forKey: Keys.doneBanners)
            registerAll()
        }
    }

    @ObservationIgnored private var online: [String: HabConnection] = [:]
    @ObservationIgnored private let defaults: UserDefaults
    @ObservationIgnored private var inFlight: [String: Task<Void, Never>] = [:]
    @ObservationIgnored private let log = Logger(subsystem: "com.flightdeck.app", category: "push")

    enum Keys {
        static let deviceID = "flightdeck.push.device-id"
        static let hideCommands = "flightdeck.push.hide-commands"
        static let doneBanners = "flightdeck.push.done-banners"
    }

    /// The setup guide a hab without push links to (app.md 8.4).
    public static let setupGuide = URL(string: "https://github.com/noahkiss/flightdeck/blob/main/docs/push.md")!

    public init(defaults: UserDefaults = .standard, environment: PushEnvironment = PushRegistrar.buildEnvironment) {
        self.defaults = defaults
        self.environment = environment
        if let id = defaults.string(forKey: Keys.deviceID), !id.isEmpty {
            deviceID = id
        } else {
            let id = UUID().uuidString.lowercased()
            defaults.set(id, forKey: Keys.deviceID)
            deviceID = id
        }
        showCommands = !defaults.bool(forKey: Keys.hideCommands)
        doneBanners = defaults.bool(forKey: Keys.doneBanners)
    }

    /// The APNs environment of this build: the `aps-environment` the build was
    /// signed with, which Info.plist repeats as `FlightdeckAPSEnvironment`
    /// (development for Debug, production for Release). Tests and command-line
    /// builds have no key: Debug means development.
    public nonisolated static var buildEnvironment: PushEnvironment {
        if let s = Bundle.main.object(forInfoDictionaryKey: "FlightdeckAPSEnvironment") as? String,
           s == "development" || s == "production" {
            return PushEnvironment(rawValue: s)
        }
        #if DEBUG
        return .development
        #else
        return .production
        #endif
    }

    /// The kinds this device asks for.
    public var kinds: [PushKind] {
        var k: [PushKind] = [.permission, .question, .auth, .error]
        if doneBanners { k.append(.done) }
        return k
    }

    /// The params `push.register` sends, or nil with no token.
    public var params: PushRegisterParams? {
        guard let token else { return nil }
        return PushRegisterParams(deviceId: deviceID, environment: environment, hideCommands: showCommands ? nil : true, kinds: kinds, token: token)
    }

    // MARK: - Inputs

    public func setPermission(_ p: Permission) {
        permission = p
    }

    /// APNs gave a token. A new one re-registers on every online hab.
    public func setDeviceToken(_ data: Data) {
        let hex = data.map { String(format: "%02x", $0) }.joined()
        tokenError = nil
        guard hex != token else { return }
        token = hex
        log.notice("push token received (\(self.environment.rawValue, privacy: .public))")
        registerAll()
    }

    public func setTokenError(_ e: String) {
        tokenError = e
    }

    /// A connection reached online with a new welcome: register there.
    public func connectionOnline(_ conn: HabConnection, welcome: Welcome) {
        guard welcome.capabilities.contains("push") else {
            online[conn.id] = nil
            status[conn.id] = .notSetUp
            return
        }
        online[conn.id] = conn
        register(conn)
    }

    /// A connection left online. Its last status stays for Settings.
    public func connectionOffline(id: String) {
        online[id] = nil
        inFlight[id]?.cancel()
        inFlight[id] = nil
    }

    /// A hab was removed from the app.
    public func forget(id: String) {
        connectionOffline(id: id)
        status[id] = nil
    }

    /// Waits for every registration in flight; for tests and for ordering the
    /// first focus report after the registration that binds the device.
    public func settle() async {
        while let t = inFlight.values.first {
            await t.value
        }
    }

    /// Waits for one hab's registration in flight, if any.
    public func settle(id: String) async {
        await inFlight[id]?.value
    }

    // MARK: - Calls

    private func registerAll() {
        for c in online.values { register(c) }
    }

    private func register(_ conn: HabConnection) {
        guard let p = params else {
            status[conn.id] = .waiting
            return
        }
        let id = conn.id
        status[id] = .registering
        let previous = inFlight[id]
        let t = Task { @MainActor [weak self] in
            await previous?.value
            do {
                _ = try await conn.call(HabMethods.pushRegister, p)
                guard let self, !Task.isCancelled else { return }
                self.status[id] = .registered
            } catch {
                guard let self, !Task.isCancelled else { return }
                if let e = error as? RPCError, e.kind == .unavailable {
                    // The hab lost its push config since the welcome.
                    self.status[id] = .notSetUp
                } else {
                    self.status[id] = .failed(String(describing: error))
                }
                self.log.notice("push.register on \(id, privacy: .private) failed: \(String(describing: error), privacy: .private)")
            }
        }
        inFlight[id] = t
        Task { @MainActor [weak self] in
            await t.value
            if self?.inFlight[id] == t { self?.inFlight[id] = nil }
        }
    }
}

extension PushRegistrar.HabPush {
    /// The line Settings shows under a hab.
    public var text: String {
        switch self {
        case .waiting: return "Waiting for this device's push token"
        case .notSetUp: return "Push is not set up on this hab"
        case .registering: return "Registering for push"
        case .registered: return "Push is on"
        case .failed(let e): return "Push registration failed: \(e)"
        }
    }
}
