import Foundation
import Observation
import SwiftUI

/// The composer's draft per session, kept on the device (app.md 4.2): leaving a session,
/// opening another, a restart or iOS ending the app leaves it in place; Send clears it.
///
/// A draft is keyed by hab and session, not pane, so a restart's or resume's new pane keeps
/// it. Without defaults (demo mode, tests) drafts live in memory only.
///
/// Each session's draft is observed on its own, so a keystroke redraws only the composer that
/// shows it. Writes to the defaults wait `saveDelay` after the last change; `flush` writes at
/// once (the app going to the background).
@MainActor
public final class DraftStore {
    /// The drafts by session key, as saved.
    public private(set) var drafts: [String: String]
    private var boxes: [String: Box] = [:]
    private let defaults: UserDefaults?
    private var pendingSave: Task<Void, Never>?
    /// How long after the last change the drafts are written.
    var saveDelay: Duration = .milliseconds(500)

    public static let key = "flightdeck.composer.drafts"

    @MainActor
    @Observable
    final class Box {
        var text: String
        init(_ text: String) { self.text = text }
    }

    public init(defaults: UserDefaults?) {
        self.defaults = defaults
        drafts = defaults?.dictionary(forKey: Self.key) as? [String: String] ?? [:]
    }

    /// The key of one session: its hab and its session id, else its pane.
    public nonisolated static func key(_ t: SessionTarget) -> String {
        if let s = t.session { return "\(t.habID)/s/\(s)" }
        return "\(t.habID)/p/\(t.pane ?? "")"
    }

    private func box(_ k: String) -> Box {
        if let b = boxes[k] { return b }
        let b = Box(drafts[k] ?? "")
        boxes[k] = b
        return b
    }

    /// The session's draft; empty when it has none.
    public subscript(_ t: SessionTarget) -> String {
        get { box(Self.key(t)).text }
        set {
            let k = Self.key(t)
            let b = box(k)
            guard b.text != newValue else { return }
            b.text = newValue
            // An empty draft is no draft: the key goes, so the store does not grow.
            drafts[k] = newValue.isEmpty ? nil : newValue
            scheduleSave()
        }
    }

    /// Drops the drafts of a hab's sessions and panes the hab no longer lists. Called with
    /// every session and pane of an online hab.
    public func prune(habID: String, sessions: Set<String>, panes: Set<String>) {
        drop { k in
            let s = k.hasPrefix(habID + "/s/"), p = k.hasPrefix(habID + "/p/")
            guard s || p else { return false }
            let id = String(k.dropFirst(habID.count + 3))
            return s ? !sessions.contains(id) : !panes.contains(id)
        }
    }

    /// Drops every draft of a hab, when it leaves the app.
    public func forget(habID: String) {
        drop { $0.hasPrefix(habID + "/") }
    }

    private func drop(_ gone: (String) -> Bool) {
        let keys = drafts.keys.filter(gone)
        guard !keys.isEmpty else { return }
        for k in keys {
            drafts[k] = nil
            boxes[k]?.text = ""
        }
        flush()
    }

    private func scheduleSave() {
        guard defaults != nil else { return }
        pendingSave?.cancel()
        let delay = saveDelay
        pendingSave = Task { [weak self] in
            try? await Task.sleep(for: delay)
            guard !Task.isCancelled else { return }
            self?.flush()
        }
    }

    /// Writes the drafts now.
    public func flush() {
        pendingSave?.cancel()
        pendingSave = nil
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
