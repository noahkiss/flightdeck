import Foundation
import HabAPI

/// Where the trust card's answer stands (app.md 7.10).
public enum TrustPhase: Equatable, Sendable {
    /// The buttons show.
    case ready
    /// `trust.answer` is on its way with this choice.
    case sending(TrustChoice)
    /// The last answer failed: the hab's reason, and the choice Retry sends again.
    case failed(TrustChoice, String)
}

/// The words of the trust card.
public enum TrustCard {
    /// The capability of a hab that serves `trust.answer`.
    public static let capability = "trust"

    public static let title = "Trust this folder?"
    public static let trustButton = "Trust folder"
    public static let declineButton = "Don't trust"
    public static let terminalLine = "Trust the folder in the terminal."

    /// What trusting allows, for the harness.
    public static func allows(_ harness: String?) -> String {
        "\(OpenRequest.harnessName(harness ?? "claude")) can read, edit and run files here."
    }

    /// The notice after "Don't trust".
    public static func declined(dropped: Int) -> String {
        dropped > 0 ? "Not trusted. The agent exited. Start message dropped." : "Not trusted. The agent exited."
    }
}

extension ChatModel {
    /// The folder trust prompt the session's agent waits on, or nil.
    public var trustPrompt: TrustPrompt? {
        guard let a = context.agent, a.state == .blocked, a.kind == .trust else { return nil }
        return a.trust ?? TrustPrompt(folder: "")
    }

    /// Whether the hab answers the trust prompt (`trust.answer`); else the card offers the terminal.
    public var canAnswerTrust: Bool {
        context.answersTrust
    }

    /// Answers the trust prompt: `trust` or `exit`. One answer at a time; a failure keeps the
    /// choice for Retry. After `exit` the notice line says the agent exited.
    public func answerTrust(_ choice: TrustChoice) async {
        guard let conn = connection, canAnswerTrust, trustPrompt != nil, let pane = context.agent?.pane ?? context.pane else { return }
        if case .sending = trustPhase { return }
        trustPhase = .sending(choice)
        do {
            let r = try await conn.call(HabMethods.trustAnswer, TrustAnswerParams(choice: choice, pane: pane))
            trustPhase = .ready
            if choice == .exit { notice = TrustCard.declined(dropped: r.dropped) }
        } catch {
            trustPhase = .failed(choice, Self.describe(error))
        }
    }

    /// Sends the failed choice again.
    public func retryTrust() async {
        guard case .failed(let choice, _) = trustPhase else { return }
        await answerTrust(choice)
    }
}
