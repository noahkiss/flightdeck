import HabAPI
import SwiftUI

extension EnvironmentValues {
    /// Opens Capcom with context attached (capcom.md 8.2, 8.4); nil offers no such action.
    @Entry public var capcom: CapcomCenter? = nil
}

/// The board's side of `CapcomCenter`: opens the Capcom session once its row exists, and
/// shows a failed open as an alert.
struct CapcomHost: ViewModifier {
    let center: CapcomCenter?
    let board: Board
    @Binding var opened: BoardRow?

    /// The row of the session the center asks for, once the board lists it.
    private var row: BoardRow? {
        guard let t = center?.openRequest else { return nil }
        return board.rows.first { $0.target.habID == t.habID && $0.target.session == t.session }
    }

    func body(content: Content) -> some View {
        content
            .onChange(of: row, initial: true) { _, r in
                guard let r else { return }
                center?.shown()
                opened = r
            }
            .alert("Could not open Capcom", isPresented: Binding(
                get: { center?.failure != nil },
                set: { if !$0 { center?.failure = nil } })) {
                Button("OK", role: .cancel) { center?.failure = nil }
            } message: {
                Text(center?.failure?.message ?? "")
            }
    }
}

/// The Capcom composer's context chips (app.md 4.9): kind symbol, title, the host when it is
/// another hab, and a remove button.
struct CapcomChipStrip: View {
    let model: ChatModel

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 6) {
                ForEach(model.pending, id: \.self) { item in
                    HStack(spacing: 4) {
                        Image(systemName: CapcomChip.symbol(item.kind))
                            .accessibilityHidden(true)
                        Text(CapcomChip.label(item, hostID: model.context.hostID))
                            .lineLimit(1)
                        Button {
                            Task { await model.detach(item) }
                        } label: {
                            Image(systemName: "xmark.circle.fill")
                        }
                        .buttonStyle(.borderless)
                        .foregroundStyle(.secondary)
                        .disabled(model.capcomBusy)
                        .accessibilityLabel("Remove \(item.title)")
                    }
                    .font(.caption)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 5)
                    .background(.quaternary.opacity(0.6), in: Capsule())
                }
            }
        }
    }
}

/// The chips a sent message carried, above its text.
struct CapcomSentChips: View {
    let items: [CapcomContextItem]
    let hostID: String

    var body: some View {
        VStack(alignment: .trailing, spacing: 2) {
            ForEach(items, id: \.self) { item in
                Label(CapcomChip.label(item, hostID: hostID), systemImage: CapcomChip.symbol(item.kind))
                    .font(.caption)
                    .lineLimit(1)
                    .foregroundStyle(.secondary)
            }
        }
    }
}
