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
    /// The rows that are messages: only they count as new on the jump button.
    var messageIDs: Set<String> = []
    /// The rows; the argument is the row the "New" divider goes above.
    @ViewBuilder var content: (_ divider: String?) -> Content

    @State private var position = ScrollPosition(idType: String.self)
    @State private var follow = ChatFollow()
    @State private var motion: ChatFollow.Motion = .idle
    /// The last distance from the bottom, kept out of the view's state: it changes every frame.
    @State private var last = LastDistance()
    @State private var divider: String?
    @State private var dividerDecided = false
    #if DEBUG
    /// Debug builds only: `-FlightdeckShowFollow YES` prints the distance and the follow state
    /// over the thread and keeps the jump button on, so a screenshot shows what the geometry reads.
    private static var showFollow: Bool { UserDefaults.standard.bool(forKey: "FlightdeckShowFollow") }
    @State private var shownDistance: CGFloat = 0
    @State private var shownGeometry = ""
    #endif

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
        #if DEBUG
        .onScrollGeometryChange(for: String.self) { g in
            guard Self.showFollow else { return "" }
            return "content \(Int(g.contentSize.height)) offset \(Int(g.contentOffset.y)) container \(Int(g.containerSize.height)) "
                + "insets \(Int(g.contentInsets.top))/\(Int(g.contentInsets.bottom)) visible \(Int(g.visibleRect.minY))-\(Int(g.visibleRect.maxY)) bounds \(Int(g.bounds.height))"
        } action: { _, t in shownGeometry = t }
        #endif
        .onScrollGeometryChange(for: ChatFollow.Sample.self) { g in
            ChatFollow.Sample(
                distance: ChatFollow.distance(contentHeight: g.contentSize.height, visibleMaxY: g.visibleRect.maxY,
                                              bottomInset: g.contentInsets.bottom).rounded(),
                contentHeight: g.contentSize.height.rounded(),
                bottomInset: g.contentInsets.bottom.rounded())
        } action: { old, new in
            last.value = new.distance
            #if DEBUG
            if Self.showFollow { shownDistance = new.distance }
            #endif
            update { $0.scrolled(from: old, to: new, motion: motion) }
        }
        .onScrollGeometryChange(for: CGFloat.self) { $0.contentInsets.bottom.rounded() } action: { old, new in
            guard old != new else { return }
            update { $0.insetChanged(motion: motion) }
        }
        .onChange(of: rowIDs, initial: true) { old, new in
            if !dividerDecided, !new.isEmpty {
                dividerDecided = true
                divider = ChatReadMarks.divider(rows: new, seen: ChatReadMarks.shared.seen(in: readKey))
            }
            update { $0.rowsChanged(from: old == new ? [] : old, to: new) { messageIDs.contains($0) } }
        }
        .onChange(of: sent) { old, new in
            guard new > old else { return }
            divider = nil
            update(animated: true) { $0.jump() }
        }
        .onChange(of: follow.following) { _, on in
            if on { ChatReadMarks.shared.saw(rowIDs.last, in: readKey) }
        }
        #if DEBUG
        .overlay(alignment: .top) {
            if Self.showFollow {
                Text("distance \(Int(shownDistance)) · \(follow.following ? "following" : "reading") · unread \(follow.unread)\n\(shownGeometry)")
                    .font(.caption.monospaced())
                    .padding(6)
                    .background(.yellow.opacity(0.8), in: Capsule())
                    .allowsHitTesting(false)
            }
        }
        #endif
        .overlay(alignment: .bottom) {
            if follow.showsJump || debugJump {
                JumpToLatest(unread: follow.unread) {
                    update(animated: true) { $0.jump() }
                }
                .transition(.scale(scale: 0.8).combined(with: .opacity))
            }
        }
        .animation(.smooth(duration: 0.2), value: follow.showsJump)
        // The first rows and the bars settle over a few frames after the thread opens.
        .task {
            try? await Task.sleep(for: .milliseconds(250))
            if follow.following { position.scrollTo(edge: .bottom) }
        }
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
                // Once more after this layout pass: the lazy rows measure only when laid out,
                // and a scroll issued before that can leave the newest rows under the bars.
                Task { @MainActor in
                    await Task.yield()
                    if follow.following { position.scrollTo(edge: .bottom) }
                }
            }
        case .keep(let id):
            position.scrollTo(id: id, anchor: .top)
        }
    }

    private var debugJump: Bool {
        #if DEBUG
        Self.showFollow
        #else
        false
        #endif
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
            // With the glass padding, at least 44 pt to tap.
            .frame(minWidth: 32, minHeight: 32)
        }
        .buttonStyle(.glass)
        .buttonBorderShape(unread > 0 ? .capsule : .circle)
        .padding(.bottom, 8)
        .accessibilityLabel(ChatFollow.jumpLabel(unread))
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
