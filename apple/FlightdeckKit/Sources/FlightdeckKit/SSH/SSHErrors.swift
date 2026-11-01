import Foundation
import NIOSSH

/// The host presented a different key from the one pinned for this hab.
/// The connection stops before authentication (app.md 10.3).
public struct HostKeyChanged: Error, CustomStringConvertible, Sendable {
    public var expected: String
    public var presented: String
    public var endpoint: String

    public var description: String {
        "the host key of \(endpoint) changed: pinned \(expected), presented \(presented). Re-pair the hab, or remove it and add it again if you trust the new key"
    }
}

/// No host key is pinned yet. The handshake stopped at the host key so the
/// user can confirm its fingerprint before the app authenticates.
public struct HostKeyUnconfirmed: Error, CustomStringConvertible, Sendable {
    public var key: NIOSSHPublicKey
    public var endpoint: String
    public var fingerprint: String { SSHFingerprint.of(key) }

    public var description: String {
        "the host key of \(endpoint) is not confirmed: \(fingerprint)"
    }
}

/// The host refused this device's key.
public struct SSHKeyRefused: Error, CustomStringConvertible, Sendable {
    public var user: String
    public var fingerprint: String

    public var description: String {
        "the host refused this device's key (\(fingerprint)) for user \(user). Add the key to that user's authorized keys on the host"
    }
}

/// TCP connected but SSH never finished its handshake.
public struct SSHHandshakeTimeout: Error, CustomStringConvertible, Sendable {
    public var endpoint: String
    public var description: String {
        "\(endpoint) accepted the connection but SSH did not start; the network may block SSH"
    }
}
