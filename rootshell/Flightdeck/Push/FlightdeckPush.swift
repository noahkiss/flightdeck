//
//  FlightdeckPush.swift
//  FlightDeck
//
//  FlightDeck pushes through its own relay, which speaks upstream's push
//  protocol. The release build sets the relay URL (ROOTSHELL_PUSH_SERVER);
//  hosts send with fd-notify, never upstream's installer.
//

import Foundation

enum FlightdeckPush {
    /// Upstream's screens append `-s -- --pair '<code>'` or
    /// `-s -- --hooks auto` to this. fd-notify's `setup` takes those
    /// arguments: it pairs the host, and leaves agent hooks alone.
    static let installCommand = "fd-notify setup"
}
