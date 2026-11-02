import FlightdeckShared
import Foundation
import HabAPI
import Observation
import os

/// Opens Capcom with context from outside its view (capcom.md 8.2, 8.4; app.md 4.9, 8.8): a
/// console command, "Ask an agent" on a notification, and "New conversation" from the chips.
/// It calls `capcom.open` with the context on the hab, hands the result's `pending` to the
/// Capcom session's chat model as chips, and asks the board to open that session. A refusal
/// shows the hab's text.
@MainActor
@Observable
public final class CapcomCenter {
    /// The Capcom session the board should open; the board clears it once shown.
    public var openRequest: SessionTarget?
    /// Why the last open failed, for an alert; the view clears it.
    public var failure: CapcomFailure?

    /// The hooks into the fleet.
    public struct Habs {
        /// The live connection of a hab, by id.
        public var connection: @MainActor (String) -> HabConnection?
        /// The chat model of a session.
        public var chat: @MainActor (SessionTarget) -> ChatModel
        /// The label the board shows for a hab.
        public var label: @MainActor (String) -> String
        /// Whether a hab is online now.
        public var online: @MainActor (String) -> Bool
        /// The hab with this device id: its welcome's, or the hab book's from an earlier run.
        public var device: @MainActor (String) -> String?

        public init(connection: @escaping @MainActor (String) -> HabConnection?,
                    chat: @escaping @MainActor (SessionTarget) -> ChatModel,
                    label: @escaping @MainActor (String) -> String,
                    online: @escaping @MainActor (String) -> Bool,
                    device: @escaping @MainActor (String) -> String?) {
            self.connection = connection
            self.chat = chat
            self.label = label
            self.online = online
            self.device = device
        }
    }

    @ObservationIgnored private let habs: Habs
    /// The model last opened, held until its view takes it: the fleet keeps models weakly.
    @ObservationIgnored public private(set) var held: ChatModel?
    /// How long "Ask an agent" waits for the Capcom host to come online after the app opens.
    @ObservationIgnored public var askWait: Duration = .seconds(20)
    /// An Ask that waits for its hab to come online.
    @ObservationIgnored private var waiting: (habID: String, context: CapcomContextRef, token: Int)?
    @ObservationIgnored private var nextToken = 0
    @ObservationIgnored let log = Logger(subsystem: "com.flightdeck.app", category: "capcom")

    public init(habs: Habs) {
        self.habs = habs
    }

    // MARK: - Opening

    /// Opens the standing Capcom conversation on a hab with `context` attached (a fresh one
    /// with `new`). Returns the Capcom session's chat model, or nil after a failure.
    @discardableResult
    public func open(habID: String, context: [CapcomContextRef], new: Bool = false) async -> ChatModel? {
        do {
            return try await run(habID: habID, context: context, new: new)
        } catch {
            failure = CapcomFailure(message: Self.message(error, label: habs.label(habID), doing: nil))
            return nil
        }
    }

    private func run(habID: String, context: [CapcomContextRef], new: Bool) async throws -> ChatModel {
        guard let conn = habs.connection(habID) else { throw TransportError("that hab is no longer in the app") }
        let r = try await conn.call(HabMethods.capcomOpen, CapcomOpenParams(context: context.isEmpty ? nil : context, new: new ? true : nil))
        return show(habID: habID, opened: r)
    }

    /// Hands an open's result to its chat model and asks the board to open it.
    func show(habID: String, opened r: CapcomOpened) -> ChatModel {
        let target = SessionTarget(habID: habID, session: r.session, pane: r.pane)
        let m = habs.chat(target)
        m.takePending(r.pending)
        held = m
        openRequest = target
        return m
    }

    /// The board showed the session: the view holds the model now.
    public func shown() {
        openRequest = nil
    }

    // MARK: - Ask an agent

    /// "Ask an agent" on a notification (capcom.md 8.2, 8.5): `capcom.open` with the push's
    /// `capcom_context` as the one context item, unread, on the hab whose device id is
    /// `capcom_home`. A hab that is not online yet gets `askWait` to come online; nothing
    /// retries after a failure. It never answers the notification's request.
    public func ask(_ meta: PushMeta) async {
        guard let c = meta.capcomContext, let home = meta.capcomHome, !home.isEmpty else {
            failure = CapcomFailure(message: "This notification names no Capcom host.")
            return
        }
        let ref = CapcomContextRef(host: c.host, kind: CapcomContextKind(rawValue: c.kind), ref: c.ref)
        guard let habID = habs.device(home) else {
            log.notice("ask: no hab with the Capcom host's device id")
            failure = CapcomFailure(message: "The hab that runs Capcom is not in this app.")
            return
        }
        guard habs.online(habID) else {
            nextToken += 1
            let token = nextToken
            waiting = (habID, ref, token)
            let wait = askWait
            Task { @MainActor [weak self] in
                try? await Task.sleep(for: wait)
                guard let self, let w = self.waiting, w.token == token else { return }
                self.waiting = nil
                self.failure = CapcomFailure(message: "Could not reach \(self.habs.label(habID)) to ask an agent.")
            }
            return
        }
        await askNow(habID: habID, ref)
    }

    private func askNow(habID: String, _ ref: CapcomContextRef) async {
        do {
            _ = try await run(habID: habID, context: [ref], new: false)
        } catch {
            failure = CapcomFailure(message: Self.message(error, label: habs.label(habID), doing: "to ask an agent"))
        }
    }

    /// The board changed: a waiting Ask goes once its hab is online.
    public func boardChanged() {
        guard let w = waiting, habs.online(w.habID) else { return }
        waiting = nil
        Task { await askNow(habID: w.habID, w.context) }
    }

    // MARK: - New conversation

    /// "New conversation" beside the chips (capcom.md 8.2): `capcom.open {new: true}` with the
    /// chips that can move, then detaches them from the old conversation, opens the new one
    /// with `draft` in its composer, and attaches the files again there. Returns false when the
    /// open was refused; the old conversation then keeps everything, with the hab's text.
    @discardableResult
    public func newConversation(from old: ChatModel, draft: String) async -> Bool {
        let habID = old.target.habID
        let moving = old.pending.filter { $0.kind != .file || old.chipFiles[$0.ref] != nil }
        let refs = moving.compactMap(old.reattachRef)
        let files = moving.compactMap { $0.kind == .file ? old.chipFiles[$0.ref] : nil }
        old.capcomBusy = true
        let m: ChatModel
        do {
            m = try await run(habID: habID, context: refs, new: true)
        } catch {
            old.capcomBusy = false
            old.notice = Self.message(error, label: habs.label(habID), doing: nil)
            return false
        }
        old.capcomBusy = false
        // The old conversation's next message must not carry them too.
        for item in moving { await old.detach(item) }
        if !draft.isEmpty { m.handoff = draft }
        for f in files { await m.attachFile(f) }
        return true
    }

    /// The text a failure shows: the hab's own words for a refusal, else "Could not reach".
    static func message(_ error: Error, label: String, doing: String?) -> String {
        if let e = error as? RPCError { return e.data?.hint ?? e.message }
        let reach = doing.map { "Could not reach \(label) \($0)." } ?? "Could not reach \(label)."
        if error is ConnectionClosed { return reach }
        return reach + " " + String(describing: error)
    }
}

/// A failed Capcom open, shown as an alert.
public struct CapcomFailure: Identifiable, Equatable, Sendable {
    public var id = UUID()
    public var message: String

    public init(message: String) {
        self.message = message
    }
}
