import Foundation
import Observation
import SwiftUI

/// The composer's draft per session, kept on the device (app.md 4.2): leaving a session,
/// opening another, a restart or iOS ending the app leaves it in place; Send clears it.
///
/// A draft is keyed by hab and session, not pane, so a restart's or resume's new pane keeps
/// it. Without defaults (demo mode) drafts live in memory only.
@MainActor
@Observable
public final class DraftStore {
    /// The drafts by session key.
    public private(set) var drafts: [String: String]
    @ObservationIgnored private let defaults: UserDefaults?

    public static let key = "flightdeck.composer.drafts"

    public init(defaults: UserDefaults?) {
        self.defaults = defaults
        drafts = defaults?.dictionary(forKey: Self.key) as? [String: String] ?? [:]
    }

    /// The key of one session: its hab and its session id, else its pane.
    public nonisolated static func key(_ t: SessionTarget) -> String {
        if let s = t.session { return "\(t.habID)/s/\(s)" }
        return "\(t.habID)/p/\(t.pane ?? "")"
    }

    /// The session's draft; empty when it has none.
    public subscript(_ t: SessionTarget) -> String {
        get { drafts[Self.key(t)] ?? "" }
        set {
            let k = Self.key(t)
            // An empty draft is no draft: the key goes, so the store does not grow.
            let v: String? = newValue.isEmpty ? nil : newValue
            guard drafts[k] != v else { return }
            drafts[k] = v
            save()
        }
    }

    /// Drops the drafts of a hab's sessions and panes the hab no longer lists. Called with
    /// every session and pane of an online hab.
    public func prune(habID: String, sessions: Set<String>, panes: Set<String>) {
        let before = drafts.count
        drafts = drafts.filter { k, _ in
            let s = k.hasPrefix(habID + "/s/"), p = k.hasPrefix(habID + "/p/")
            guard s || p else { return true }
            let id = String(k.dropFirst(habID.count + 3))
            return s ? sessions.contains(id) : panes.contains(id)
        }
        if drafts.count != before { save() }
    }

    /// Drops every draft of a hab, when it leaves the app.
    public func forget(habID: String) {
        let before = drafts.count
        drafts = drafts.filter { !$0.key.hasPrefix(habID + "/") }
        if drafts.count != before { save() }
    }

    private func save() {
        guard let defaults else { return }
        if drafts.isEmpty {
            defaults.removeObject(forKey: Self.key)
        } else {
            defaults.set(drafts, forKey: Self.key)
        }
    }
}

extension EnvironmentValues {
    /// The composer's drafts (app.md 4.2); nil keeps each draft in its view.
    @Entry public var drafts: DraftStore? = nil
}
