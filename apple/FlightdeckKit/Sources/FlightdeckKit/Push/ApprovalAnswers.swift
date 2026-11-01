import Foundation
import FlightdeckShared
import HabAPI

/// The notification categories and actions Flightdeck registers (app.md 7.1,
/// 8.6, 16.2). The hab names the category in each push.
public enum PushCategory {
    /// Approve and Deny: the whole command fits the banner.
    public static let approval = "approval"
    /// Deny and Open: the command does not fit, or commands are hidden.
    public static let approvalDeny = "approval-deny"
    /// Open only: questions, sign-in, errors, a request with no id.
    public static let open = "open"
    /// A question with a text Reply. The hab does not send it until it answers
    /// questions through `approval.answer`; the app is ready for it.
    public static let question = "question"

    public static let approveAction = "flightdeck.approve"
    public static let denyAction = "flightdeck.deny"
    public static let openAction = "flightdeck.open"
    public static let replyAction = "flightdeck.reply"
}

/// What a tap on a notification asks for.
public enum NotificationIntent: Equatable, Sendable {
    /// Answer the request in the background.
    case answer(host: String, ApprovalAnswerParams)
    /// Open the app at the request or session.
    case open(PushMeta)
    /// Nothing to do (dismissed, or a push without our object).
    case none
}

public enum NotificationAnswer {
    /// Maps a notification response to what the app does. An answer needs the
    /// pane and the request id the push carried; without them the tap opens
    /// the app instead (app.md 7.8).
    public static func intent(action: String, meta: PushMeta?, text: String? = nil) -> NotificationIntent {
        guard let meta else { return .none }
        let choice: ApprovalChoice
        switch action {
        case PushCategory.approveAction: choice = .allow
        case PushCategory.denyAction: choice = .deny
        case PushCategory.replyAction:
            guard let t = text?.trimmingCharacters(in: .whitespacesAndNewlines), !t.isEmpty else { return .open(meta) }
            guard let pane = meta.pane, let request = meta.request, !request.isEmpty else { return .open(meta) }
            return .answer(host: meta.hab, ApprovalAnswerParams(choice: .answer, pane: pane, request: request, text: t, via: .banner))
        case PushCategory.openAction, defaultAction:
            return .open(meta)
        default:
            return .none
        }
        guard let pane = meta.pane, let request = meta.request, !request.isEmpty else { return .open(meta) }
        return .answer(host: meta.hab, ApprovalAnswerParams(choice: choice, pane: pane, request: request, via: .banner))
    }

    /// `UNNotificationDefaultActionIdentifier`, spelled out so this file needs
    /// no UserNotifications import.
    public static let defaultAction = "com.apple.UNNotificationDefaultActionIdentifier"
}

/// How an answer went.
public enum AnswerOutcome: Equatable, Sendable {
    case answered
    /// The hab refused it with `conflict`: answered elsewhere, or expired.
    case refused(String)
    /// The hab took the call and could not deliver it (`unavailable` and the like).
    case failed(String)
    /// The hab could not be reached: no route, a dial failure or a timeout.
    case unreachable(String)

    public static func classify(_ error: Error) -> AnswerOutcome {
        if let e = error as? RPCError, let kind = e.kind {
            let hint = e.data?.hint ?? e.message
            if kind == .conflict { return .refused(hint) }
            return .failed(hint)
        }
        return .unreachable(String(describing: error))
    }

    /// Whether the app should raise the request again in the foreground.
    public var reraise: Bool {
        switch self {
        case .failed, .unreachable: return true
        case .answered, .refused: return false
        }
    }

    /// The local notification that reports a background answer that did not
    /// land (app.md 7.5, 7.8, 16.2 "Could not reach", 16.3), or nil.
    public func followUp(hab: String, expiry: Date?, now: Date = Date()) -> (title: String, body: String)? {
        switch self {
        case .answered:
            return nil
        case .refused(let hint):
            if let expiry, expiry <= now {
                return ("Expired: answer in the terminal", hab)
            }
            return ("Already answered", hint.isEmpty ? hab : hint)
        case .failed(let hint):
            return ("Could not answer on \(hab)", hint)
        case .unreachable:
            return ("Could not reach \(hab)", Self.openToRetry)
        }
    }

    static var openToRetry: String {
        #if os(iOS)
        return "Open on iPhone to retry."
        #else
        return "Open Flightdeck to retry."
        #endif
    }
}

/// Sends one `approval.answer`: on the live connection when the hab is online
/// in this process, else on a one-shot connection of its own (`HabOneShot`),
/// which is the path for an action that woke the app in the background. Fails
/// fast and never retries (app.md 18.7).
public struct ApprovalAnswerer: Sendable {
    /// The live connection for a host id, when one is online.
    public var live: @MainActor @Sendable (_ host: String) async -> HabConnection?
    /// A transport for a host id, for a one-shot call; throws why there is none.
    public var oneShot: @Sendable (_ host: String) async throws -> Transport
    public var clientLabel: String
    public var deadline: Duration

    public init(live: @escaping @MainActor @Sendable (String) async -> HabConnection?, oneShot: @escaping @Sendable (String) async throws -> Transport, clientLabel: String, deadline: Duration = .seconds(20)) {
        self.live = live
        self.oneShot = oneShot
        self.clientLabel = clientLabel
        self.deadline = deadline
    }

    public func answer(host: String, _ params: ApprovalAnswerParams) async -> AnswerOutcome {
        if let conn = await live(host), await conn.state.status == .online {
            do {
                _ = try await conn.call(HabMethods.approvalAnswer, params)
                return .answered
            } catch {
                let o = AnswerOutcome.classify(error)
                // The live connection died under the call: try once on a fresh one.
                guard case .unreachable = o else { return o }
            }
        }
        do {
            let t = try await oneShot(host)
            _ = try await HabOneShot.call(t, HabMethods.approvalAnswer, params, clientLabel: clientLabel, deadline: deadline)
            return .answered
        } catch {
            return AnswerOutcome.classify(error)
        }
    }
}
