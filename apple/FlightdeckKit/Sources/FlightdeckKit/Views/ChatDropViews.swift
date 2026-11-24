import SwiftUI

/// Drop a file on a chat (app.md 4.4), on iPad and the Mac: the files join the composer's
/// attachments, text and links join the draft, and a highlight shows while a drag is over the
/// chat. A session chat and a Capcom chat both take it; a Capcom chat turns the files into
/// context chips, as its attach button does.
struct ChatDropTarget: ViewModifier {
    /// The composer takes files now.
    let enabled: Bool
    @Binding var draft: String
    @Binding var attachments: [DraftAttachment]
    @Binding var notice: String?
    @State private var targeted = false

    func body(content: Content) -> some View {
        content
            // The overload with `isTargeted`: the DropSession form gives no highlight on iOS
            // before 27 (`onDropSessionUpdated`), and the app's floor is 26.1.
            .dropDestination(for: ChatDropItem.self) { items, _ in
                guard enabled, !items.isEmpty else { return false }
                Task { @MainActor in
                    let out = await ChatDrop.prepare(items)
                    attachments += out.attachments
                    draft = ChatDrop.draft(draft, adding: out.text)
                    notice = out.notice
                }
                return true
            } isTargeted: { over in
                targeted = over
            }
            .overlay {
                if targeted && enabled {
                    DropHighlight()
                        .transition(.opacity)
                }
            }
            .animation(.easeOut(duration: 0.15), value: targeted && enabled)
    }
}

/// The highlight over a chat while a drag is over it.
private struct DropHighlight: View {
    var body: some View {
        RoundedRectangle(cornerRadius: 20, style: .continuous)
            .strokeBorder(.tint, lineWidth: 3)
            .background(.tint.opacity(0.08), in: RoundedRectangle(cornerRadius: 20, style: .continuous))
            .overlay {
                Label("Drop to attach", systemImage: "paperclip")
                    .font(.headline)
                    .padding(.horizontal, 16)
                    .padding(.vertical, 10)
                    .glassEffect(.regular, in: .capsule)
            }
            .padding(6)
            .allowsHitTesting(false)
            .accessibilityHidden(true)
    }
}
