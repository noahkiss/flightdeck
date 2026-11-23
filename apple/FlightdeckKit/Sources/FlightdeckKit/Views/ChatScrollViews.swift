import SwiftUI

/// The scrolling list under a chat (app.md 4.12): the session chat and the Capcom thread. It opens
/// at the newest row and follows it while the owner is at the bottom, by the scroll geometry
/// (`ChatFollow`), not a marker row. Scrolled up, it shows a jump-to-latest button with the count
/// of new rows, and a thread opened again marks where the owner left off with a "New" divider.
struct ChatScroll<Content: View>: View {
    /// The rows' ids, in order. Rows that only come and go (the typing mark, messages on their way)
    /// stay out: they follow by the geometry.
    let rowIDs: [String]
    /// The thread's key in `ChatReadMarks`.
    let readKey: String
    /// Grows when the owner sends: the thread shows the newest row.
    let sent: Int
    /// The rows; the argument is the row the "New" divider goes above.
    @ViewBuilder var content: (_ divider: String?) -> Content

    @State private var position = ScrollPosition(idType: String.self)
    @State private var follow = ChatFollow()
    @State private var motion: ChatFollow.Motion = .idle
    /// The last distance from the bottom, kept out of the view's state: it changes every frame.
    @State private var last = LastDistance()
    @State private var divider: String?
    @State private var dividerDecided = false

    var body: some View {
        ScrollView {
            LazyVStack(alignment: .leading, spacing: 12) {
                content(divider)
            }
            .scrollTargetLayout()
            .padding(12)
        }
        .scrollPosition($position)
        .defaultScrollAnchor(.bottom)
        // A streaming reply or a growing tool card keeps the bottom in view.
        .defaultScrollAnchor(.bottom, for: .sizeChanges)
        .modifier(DismissesKeyboard())
        .onScrollPhaseChange { _, phase in
            switch phase {
            case .idle:
                motion = .idle
                // The content may have grown while the finger held the thread.
                update { $0.scrolled(distance: last.value, motion: .idle) }
            case .animating: motion = .animating
            default: motion = .user
            }
        }
        // The bars over the thread (composer, keyboard, request card) sit in its insets, so the
        // distance counts them; a following thread goes back to the bottom when they change,
        // but never while a finger moves it (the drag that takes the keyboard away).
        .onScrollGeometryChange(for: CGFloat.self) { g in
            ChatFollow.distance(contentHeight: g.contentSize.height, offsetY: g.contentOffset.y,
                                containerHeight: g.containerSize.height,
                                topInset: g.contentInsets.top, bottomInset: g.contentInsets.bottom).rounded()
        } action: { _, d in
            last.value = d
            update { $0.scrolled(distance: d, motion: motion) }
        }
        .onChange(of: rowIDs, initial: true) { old, new in
            if !dividerDecided, !new.isEmpty {
                dividerDecided = true
                divider = ChatReadMarks.divider(rows: new, seen: ChatReadMarks.shared.seen(in: readKey))
            }
            update { $0.rowsChanged(from: old == new ? [] : old, to: new) }
        }
        .onChange(of: sent) { old, new in
            guard new > old else { return }
            divider = nil
            update(animated: true) { $0.jump() }
        }
        .onChange(of: follow.following) { _, on in
            if on { ChatReadMarks.shared.saw(rowIDs.last, in: readKey) }
        }
        .overlay(alignment: .bottom) {
            if follow.showsJump {
                JumpToLatest(unread: follow.unread) {
                    update(animated: true) { $0.jump() }
                }
                .transition(.scale(scale: 0.8).combined(with: .opacity))
            }
        }
        .animation(.smooth(duration: 0.2), value: follow.showsJump)
        .onDisappear {
            if follow.following { ChatReadMarks.shared.saw(rowIDs.last, in: readKey) }
        }
    }

    /// Runs one input through the follow state, writes the state only when it changed, and does
    /// what it asks.
    private func update(animated: Bool = false, _ input: (inout ChatFollow) -> ChatFollow.Action) {
        var f = follow
        let action = input(&f)
        if f != follow { follow = f }
        if f.following { ChatReadMarks.shared.saw(rowIDs.last, in: readKey) }
        switch action {
        case .none: break
        case .toBottom:
            if animated {
                withAnimation(.smooth(duration: 0.3)) { position.scrollTo(edge: .bottom) }
            } else {
                position.scrollTo(edge: .bottom)
            }
        case .keep(let id):
            position.scrollTo(id: id, anchor: .top)
        }
    }

    private final class LastDistance {
        var value: CGFloat = 0
    }
}

/// The jump-to-latest button over the bottom of the thread, with the count of new rows.
struct JumpToLatest: View {
    let unread: Int
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: 6) {
                Image(systemName: "arrow.down")
                    .font(.footnote.weight(.bold))
                if unread > 0 {
                    Text(ChatFollow.jumpLabel(unread))
                        .font(.footnote.weight(.semibold))
                        .contentTransition(.numericText(value: Double(unread)))
                }
            }
            .padding(.horizontal, unread > 0 ? 6 : 0)
            .frame(minWidth: 28, minHeight: 28)
        }
        .buttonStyle(.glass)
        .buttonBorderShape(unread > 0 ? .capsule : .circle)
        .padding(.bottom, 8)
        .accessibilityLabel(ChatFollow.jumpLabel(unread))
        .keyboardShortcut(.downArrow, modifiers: .command)
    }
}

/// "New" over the first row that arrived since the owner last saw the thread.
struct UnreadDivider: View {
    var body: some View {
        HStack(spacing: 8) {
            VStack { Divider().overlay(Color.accentColor) }
            Text("New")
                .font(.caption.weight(.semibold))
                .foregroundStyle(Color.accentColor)
                .fixedSize()
            VStack { Divider().overlay(Color.accentColor) }
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("New messages")
    }
}
