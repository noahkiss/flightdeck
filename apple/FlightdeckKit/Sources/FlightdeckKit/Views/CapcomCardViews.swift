import HabAPI
import SwiftUI

/// Capcom's `hand_off` call: where the message went, how delivery stands, why that agent, the
/// other candidates and the text sent (app.md 4.8). A record; it asks nothing.
struct HandOffCardView: View {
    let card: HandOffCard
    @State private var fullMessage = false

    /// Lines of the sent text shown before "Show full message".
    static let messageLines = 3

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack(spacing: 6) {
                if card.delivery == .sending {
                    ProgressView().controlSize(.mini)
                } else {
                    Image(systemName: symbol).foregroundStyle(tint).font(.caption)
                }
                Text("Hand-off").font(.callout.weight(.medium))
                Spacer(minLength: 0)
                Text(card.deliveryWord)
                    .font(.caption.weight(.medium))
                    .foregroundStyle(tint)
            }
            .accessibilityElement(children: .combine)
            if let t = card.target {
                HStack(alignment: .firstTextBaseline, spacing: 4) {
                    Text(t.address).font(.callout.monospaced())
                    if let h = t.host {
                        Text("on \(h)").font(.callout).foregroundStyle(.secondary)
                    }
                    Text("· \(harnessName(t.harness)), \(t.state)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                .lineLimit(1)
                .truncationMode(.middle)
            } else if let p = card.named ?? card.project {
                Text(p + (card.host.map { " on \($0)" } ?? "")).font(.callout.monospaced()).foregroundStyle(.secondary).lineLimit(1)
            }
            if let l = card.line {
                Text(l).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            }
            if let w = card.why {
                Text("Why: \(w)").font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
            }
            if let a = card.answer {
                Text(a)
                    .font(.caption.monospaced())
                    .foregroundStyle(card.delivery == .error ? Color.red : Color.secondary)
                    .textSelection(.enabled)
            }
            if !card.others.isEmpty {
                VStack(alignment: .leading, spacing: 2) {
                    Text("Also on the project").font(.caption2).foregroundStyle(.tertiary)
                    ForEach(card.others, id: \.address) { o in
                        Text("\(o.address) · \(harnessName(o.harness)), \(o.state)")
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                    }
                }
            }
            if !card.message.isEmpty {
                message
            }
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 8))
        .overlay(alignment: .leading) {
            Rectangle().fill(tint.opacity(0.6)).frame(width: 3).clipShape(RoundedRectangle(cornerRadius: 1.5))
        }
    }

    @ViewBuilder private var message: some View {
        let lines = card.message.components(separatedBy: "\n")
        let long = lines.count > Self.messageLines || card.message.count > 280
        VStack(alignment: .leading, spacing: 4) {
            Text(card.message)
                .font(.callout)
                .lineLimit(fullMessage || !long ? nil : Self.messageLines)
                .textSelection(.enabled)
            if long {
                Button(fullMessage ? "Show less" : "Show full message") {
                    withAnimation(.easeOut(duration: 0.15)) { fullMessage.toggle() }
                }
                .font(.caption)
            }
        }
        .padding(8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.background.opacity(0.6), in: RoundedRectangle(cornerRadius: 6))
    }

    private var symbol: String {
        switch card.delivery {
        case .delivered: return "checkmark.circle.fill"
        case .queued: return "clock"
        case .held: return "tray.full"
        case .failed, .error: return "xmark.circle.fill"
        case .notSent: return "minus.circle"
        case .sending: return "arrow.turn.up.right"
        }
    }

    private var tint: Color {
        switch card.delivery {
        case .delivered: return .green
        case .queued, .held: return .orange
        case .failed, .error: return .red
        case .notSent, .sending: return .secondary
        }
    }
}

/// Capcom's `file_record` call: the record it proposed and whether it waits for review
/// (app.md 4.8). Review is an approval; this line has no buttons.
struct ProposalLineView: View {
    let line: ProposalLine
    @State private var open = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            Button {
                withAnimation(.easeOut(duration: 0.15)) { open.toggle() }
            } label: {
                HStack(spacing: 6) {
                    if line.state == .filing {
                        ProgressView().controlSize(.mini)
                    } else {
                        Image(systemName: line.state == .notFiled ? "xmark.circle" : "tray.and.arrow.down")
                            .foregroundStyle(line.state == .notFiled ? Color.red : Color.secondary)
                            .font(.caption)
                    }
                    Text("Proposal").font(.callout.weight(.medium))
                    Text("\(line.type): \(line.title)")
                        .font(.callout)
                        .lineLimit(1)
                        .truncationMode(.tail)
                    Spacer(minLength: 0)
                    Text(line.stateText)
                        .font(.caption)
                        .foregroundStyle(line.state == .notFiled ? Color.red : Color.secondary)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityHint(open ? "Folds the proposal" : "Shows the proposal")
            if open {
                VStack(alignment: .leading, spacing: 6) {
                    if let p = line.project {
                        Text("Project: \(p)").font(.caption).foregroundStyle(.secondary)
                    }
                    if let q = line.quote {
                        Text(q)
                            .font(.caption)
                            .foregroundStyle(.secondary)
                            .padding(.leading, 8)
                            .overlay(alignment: .leading) { Rectangle().fill(.tertiary).frame(width: 2) }
                    }
                    if let b = line.body {
                        MarkdownView(blocks: MarkdownBlock.parse(b))
                    }
                    if let a = line.answer {
                        Text(a).font(.caption.monospaced()).foregroundStyle(.secondary)
                    }
                }
                .textSelection(.enabled)
                .padding(.leading, 16)
            }
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 7)
        .background(.quaternary.opacity(0.25), in: RoundedRectangle(cornerRadius: 8))
    }
}
