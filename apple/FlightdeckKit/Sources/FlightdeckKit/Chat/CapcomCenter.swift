import Foundation
import HabAPI
import Observation
import os

/// Opens Capcom with context from outside its view (capcom.md 8.2, 8.4; app.md 4.9): a
/// console command now. It calls `capcom.open` with the context on the hab, hands the result's
/// `pending` to the Capcom session's chat model as chips, and asks the board to open that
/// session. A refusal shows the hab's text.
@MainActor
@Observable
public final class CapcomCenter {
    /// The Capcom session the board should open; the board clears it once shown.
    public var openRequest: SessionTarget?
    /// Why the last open failed, for an alert; the view clears it.
    public var failure: CapcomFailure?

    @ObservationIgnored private let connection: @MainActor (String) -> HabConnection?
    @ObservationIgnored private let chat: @MainActor (SessionTarget) -> ChatModel
    @ObservationIgnored private let label: @MainActor (String) -> String
    /// The model last opened, held until its view takes it: the fleet keeps models weakly.
    @ObservationIgnored public private(set) var held: ChatModel?
    @ObservationIgnored let log = Logger(subsystem: "com.flightdeck.app", category: "capcom")

    public init(connection: @escaping @MainActor (String) -> HabConnection?,
                chat: @escaping @MainActor (SessionTarget) -> ChatModel,
                label: @escaping @MainActor (String) -> String) {
        self.connection = connection
        self.chat = chat
        self.label = label
    }

    /// Opens the standing Capcom conversation on a hab with `context` attached (a fresh one
    /// with `new`). Returns the Capcom session's chat model, or nil after a failure.
    @discardableResult
    public func open(habID: String, context: [CapcomContextRef], new: Bool = false) async -> ChatModel? {
        guard let conn = connection(habID) else {
            failure = CapcomFailure(message: "That hab is no longer in the app.")
            return nil
        }
        do {
            let r = try await conn.call(HabMethods.capcomOpen, CapcomOpenParams(context: context.isEmpty ? nil : context, new: new ? true : nil))
            return show(habID: habID, opened: r)
        } catch is ConnectionClosed {
            failure = CapcomFailure(message: "Could not reach \(label(habID)).")
        } catch let e as RPCError {
            failure = CapcomFailure(message: e.data?.hint ?? e.message)
        } catch {
            failure = CapcomFailure(message: "Could not reach \(label(habID)): \(error)")
        }
        return nil
    }

    /// Hands an open's result to its chat model and asks the board to open it.
    func show(habID: String, opened r: CapcomOpened) -> ChatModel {
        let target = SessionTarget(habID: habID, session: r.session, pane: r.pane)
        let m = chat(target)
        m.takePending(r.pending)
        held = m
        openRequest = target
        return m
    }

    /// The board showed the session: the view holds the model now.
    public func shown() {
        openRequest = nil
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
