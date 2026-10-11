//
//  TailnetController.swift
//  rootshell
//
//  Routes status, sign-in and sign-out to whichever Tailscale is in use: the
//  VPN extension (Whole Device) or the in-app engine (rootshell Only).
//

#if !CHINA_BUILD

import Foundation

@MainActor
enum TailnetController {
    static var mode: TailnetMode { TailnetModeSettings.load().effectiveMode }

    /// The engine for the current mode is up and can answer.
    static var isActive: Bool {
        switch mode {
        case .wholeDevice:
            let vpn = VPNManager.shared
            return vpn.isVPNActive(for: VPNTailnetProfile.id) && vpn.isTunnelUp
        case .rootshellOnly:
            return TailnetInAppEngine.shared.isStarted
        }
    }

    static func status() async -> TailnetStatus? {
        switch mode {
        case .wholeDevice:
            return await VPNManager.shared.tailnetStatus()
        case .rootshellOnly:
            let engine = TailnetInAppEngine.shared
            await engine.refreshStatus()
            return engine.status
        }
    }

    /// Asks for a fresh login URL; returns an error message on failure.
    static func login() async -> String? {
        #if FLIGHTDECK
        return "Tailscale is disabled in flightdeck."
        #endif
        switch mode {
        case .wholeDevice: return await VPNManager.shared.tailnetLogin()
        case .rootshellOnly: return await TailnetInAppEngine.shared.login()
        }
    }

    /// Logs this device out of the tailnet; returns an error message on failure.
    static func logout() async -> String? {
        switch mode {
        case .wholeDevice: return await VPNManager.shared.tailnetLogout()
        case .rootshellOnly: return await TailnetInAppEngine.shared.logout()
        }
    }
}

#endif
