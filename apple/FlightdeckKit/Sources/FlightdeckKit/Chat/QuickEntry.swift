import Foundation
import Observation

/// Quick entry for Capcom (app.md 3.18): a message to Capcom's standing conversation, or to a
/// new one, from the Mac's floating panel. It opens the conversation, waits until it takes a
/// message and until the hab takes the send, and keeps the text when the send fails. A send
/// the hab never answered may still arrive, so its text moves to the recents instead.
@MainActor
@Observable
public final class QuickEntry {
    /// Where a message would go now.
    public enum Readiness: Equatable, Sendable {
        /// Capcom's home is online.
        case ready(habID: String, label: String)
        /// The habs connect, or the home is not known yet.
        case waiting(String)
        /// It cannot go: no home, the home offline, or not in the app.
        case unavailable(String)
    }

    /// The text in the panel's field. It stays while the app runs.
    public var text = ""
    /// The last messages sent from the panel, newest first.
    public private(set) var recents: [String]
    /// A send is on its way.
    public private(set) var sending = false {
        didSet { if sending != oldValue { onSending?(sending) } }
    }
    /// Why the last send failed; nil after a success or an edit.
    public var failure: String?

    /// Told when a send starts and ends; the panel's window count holds while one runs.
    @ObservationIgnored var onSending: ((Bool) -> Void)?
    @ObservationIgnored private let capcom: CapcomCenter
    @ObservationIgnored private let defaults: UserDefaults
    /// How long a send waits for the conversation to take a message, and then for the hab.
    @ObservationIgnored public var wait: Duration = .seconds(15)

    /// The most recent messages kept.
    public static let maxRecents = 5
    static let recentsKey = "flightdeck.quickEntry.recents"

    public init(capcom: CapcomCenter, defaults: UserDefaults = .standard) {
        self.capcom = capcom
        self.defaults = defaults
        recents = defaults.stringArray(forKey: Self.recentsKey) ?? []
    }

    public var readiness: Readiness {
        switch capcom.entry {
        case .ready(let id):
            return .ready(habID: id, label: capcom.habs.label(id))
        case .loading:
            return .waiting("Connecting…")
        case .noHabOnline:
            return capcom.habs.statuses().isEmpty ? .unavailable("No paired computer.") : .waiting("Connecting…")
        case .unassigned:
            return .unavailable("No Capcom host. Choose one in Settings.")
        case .offline(let id):
            return .unavailable("\(capcom.habs.label(id)) is offline")
        case .notInApp:
            return .unavailable("Capcom's host is not in this app.")
        }
    }

    /// The line under the field: where the message goes, or why it cannot yet.
    public var line: String {
        switch readiness {
        case .ready(_, let label): return "To Capcom on \(label)"
        case .waiting(let why), .unavailable(let why): return why
        }
    }

    private var trimmed: String { text.trimmingCharacters(in: .whitespacesAndNewlines) }

    /// Send is on: Capcom's home is online, the field holds text, and no send runs.
    public var canSend: Bool {
        guard case .ready = readiness else { return false }
        return !sending && !trimmed.isEmpty
    }

    /// Sends the text to the standing conversation, or to a new one with `new`. Returns true
    /// once the hab took it; the field then empties and the text joins the recents. On failure,
    /// a pane-based Capcom mid-turn included, the text stays and `failure` says why; with no
    /// answer in time the text moves to the recents.
    @discardableResult
    public func send(new: Bool = false) async -> Bool {
        guard canSend, case .ready(let habID, let label) = readiness else { return false }
        let message = trimmed
        sending = true
        failure = nil
        defer { sending = false }
        let model: ChatModel
        do {
            model = try await capcom.run(habID: habID, context: [], new: new, request: false)
        } catch {
            failure = CapcomCenter.message(error, label: label, doing: nil)
            return false
        }
        // The conversation's state reaches the app after the open: wait until it takes a message.
        guard await until({ model.takesMessage }) else {
            failure = "Capcom did not take the message."
            return false
        }
        let before = Set(model.outgoing.map(\.id))
        model.send(message, origin: .typed)
        guard let id = model.outgoing.last(where: { !before.contains($0.id) })?.id else {
            failure = model.notice ?? "Capcom did not take the message."
            return false
        }
        // Wait for the hab's answer to the send.
        _ = await until { model.outgoing.first(where: { $0.id == id })?.status != .sending }
        switch model.outgoing.first(where: { $0.id == id })?.status {
        case .failed(let why)?:
            // The panel keeps the text; the Capcom view would show it again on a retry.
            model.dismiss(id)
            failure = why
            return false
        case .sending?:
            // The message may still arrive: the field empties so Return cannot send it twice,
            // and Recent keeps it.
            remember(message)
            text = ""
            failure = "Capcom has not confirmed it yet. It may still arrive: check Capcom before you send it again."
            return false
        case .queued?:
            // A pane-based Capcom mid-turn: the message would wait in the app, which may
            // disconnect once the panel closes (12.1). Keep it in the panel instead.
            model.dismiss(id)
            failure = "Capcom is busy with a turn. Send again when it finishes."
            return false
        default:
            remember(message)
            text = ""
            return true
        }
    }

    /// Puts a recent message back in the field.
    public func pick(_ recent: String) {
        text = recent
        failure = nil
    }

    public func clearRecents() {
        recents = []
        defaults.removeObject(forKey: Self.recentsKey)
    }

    private func remember(_ message: String) {
        recents.removeAll { $0 == message }
        recents.insert(message, at: 0)
        if recents.count > Self.maxRecents { recents.removeLast(recents.count - Self.maxRecents) }
        defaults.set(recents, forKey: Self.recentsKey)
    }

    /// Waits until `cond` holds, up to `wait`; false when it never did.
    private func until(_ cond: () -> Bool) async -> Bool {
        let end = ContinuousClock.now.advanced(by: wait)
        while !cond() {
            guard ContinuousClock.now < end else { return false }
            try? await Task.sleep(for: .milliseconds(50))
        }
        return true
    }
}

/// The quick entry panel's place in the Mac's window count (app.md 3.18, 12.1), and the
/// shortcut's debounce. The panel counts while it shows and while a send it started runs, so
/// closing it mid-send does not disconnect the habs under that send.
@MainActor
public final class QuickEntryWindow {
    private let opened: () -> Void
    private let closed: () -> Void
    /// The panel shows.
    public private(set) var shows = false
    /// A send runs.
    public private(set) var sending = false
    private var counted = false
    private var lastToggle: ContinuousClock.Instant?

    /// A second shortcut press within this gap is ignored: KeyboardShortcuts 3.1.0 can fire
    /// twice while a menu is open (its issue 243).
    public static let toggleGap: Duration = .milliseconds(150)

    public convenience init(fleet: Fleet, entry: QuickEntry) {
        self.init(entry: entry, opened: { fleet.windowOpened() }, closed: { fleet.windowClosed() })
    }

    init(entry: QuickEntry, opened: @escaping () -> Void, closed: @escaping () -> Void) {
        self.opened = opened
        self.closed = closed
        entry.onSending = { [weak self, weak entry] on in
            guard let self else { return }
            self.sending = on
            // A send that failed after the panel closed (Esc or the shortcut): show it again.
            if !on, !self.shows, entry?.failure != nil { self.onFailedWhileHidden?() }
            self.update()
        }
    }

    /// Called when a send ends failed while the panel is closed, so the panel shows the
    /// failure again.
    public var onFailedWhileHidden: (() -> Void)?

    /// The panel counts while it shows, or while a send runs.
    public var counts: Bool { counted }

    /// A click elsewhere closes the panel, except while a send runs: the panel stays to show
    /// how the send ended.
    public var closesOnResign: Bool { !sending }

    public func shown() {
        shows = true
        update()
    }

    public func hidden() {
        shows = false
        update()
    }

    /// The shortcut fired at `now`: false when it is the second of a double fire.
    public func acceptsToggle(at now: ContinuousClock.Instant = .now) -> Bool {
        if let last = lastToggle, now - last < Self.toggleGap { return false }
        lastToggle = now
        return true
    }

    private func update() {
        let want = shows || sending
        guard want != counted else { return }
        counted = want
        if want { opened() } else { closed() }
    }
}
