import HabAPI
import SwiftUI

/// The folder trust card (app.md 7.10), shown above the composer while the agent waits on its
/// folder trust prompt, where the pending request card goes. "Don't trust" exits the agent.
struct TrustCardView: View {
    let model: ChatModel
    var openTerminal: (() -> Void)?

    var body: some View {
        let prompt = model.trustPrompt
        VStack(alignment: .leading, spacing: 6) {
            Label(TrustCard.title, systemImage: "folder.badge.questionmark")
                .font(.callout.weight(.medium))
                .foregroundStyle(.orange)
            if let folder = prompt?.folder, !folder.isEmpty {
                Text(folder)
                    .font(.caption.monospaced())
                    .textSelection(.enabled)
            }
            Text(TrustCard.allows(model.context.agent?.harness))
                .font(.caption)
                .foregroundStyle(.secondary)
            actions
        }
        .padding(8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.orange.opacity(0.1), in: RoundedRectangle(cornerRadius: 8))
    }

    @ViewBuilder
    private var actions: some View {
        if !model.canAnswerTrust {
            HStack {
                Text(TrustCard.terminalLine).font(.caption).foregroundStyle(.secondary)
                Spacer()
                if let openTerminal { Button("Open terminal", action: openTerminal) }
            }
        } else {
            switch model.trustPhase {
            case .sending:
                ProgressView().frame(maxWidth: .infinity)
            case .failed(_, let why):
                Text(why).font(.caption).foregroundStyle(.red)
                HStack(spacing: 10) {
                    buttons
                    Button("Retry") { Task { await model.retryTrust() } }
                        .buttonStyle(.bordered)
                }
            case .ready:
                HStack(spacing: 10) { buttons }
            }
        }
    }

    @ViewBuilder
    private var buttons: some View {
        Button(role: .destructive) {
            Task { await model.answerTrust(.exit) }
        } label: {
            Text(TrustCard.declineButton).frame(maxWidth: .infinity)
        }
        .buttonStyle(.bordered)
        Button {
            Task { await model.answerTrust(.trust) }
        } label: {
            Text(TrustCard.trustButton).frame(maxWidth: .infinity)
        }
        .buttonStyle(.borderedProminent)
    }
}
