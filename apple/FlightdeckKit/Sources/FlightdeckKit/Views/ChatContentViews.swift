import HabAPI
import SwiftUI

// The chat view's content (app.md 4.11): tool cards as sentences, diffs, sideways scrolling,
// folded hidden steps, and the pinned banners.

// MARK: - Colours

/// Diff and count colours that pass 4.5:1 (WCAG AA) in light and dark (app.md 4.11).
struct DiffPalette {
    let dark: Bool

    init(_ scheme: ColorScheme) { dark = scheme == .dark }

    var removedBackground: Color { dark ? Color(hex: 0x3D1F1D) : Color(hex: 0xFDECEA) }
    var removedText: Color { dark ? Color(hex: 0xFFB4AB) : Color(hex: 0x8C1D18) }
    var addedBackground: Color { dark ? Color(hex: 0x15301E) : Color(hex: 0xE7F5EA) }
    var addedText: Color { dark ? Color(hex: 0xA7E8B5) : Color(hex: 0x14532D) }
    var gutter: Color { dark ? Color(hex: 0xA0A0AA) : Color(hex: 0x5C5C66) }
    /// Counts in a header: "+3" and "-1".
    var addedCount: Color { dark ? Color(hex: 0x6FD08C) : Color(hex: 0x1E7B3A) }
    var removedCount: Color { dark ? Color(hex: 0xFF8A80) : Color(hex: 0xB3261E) }
}

extension Color {
    init(hex: UInt32) {
        self.init(red: Double((hex >> 16) & 0xFF) / 255, green: Double((hex >> 8) & 0xFF) / 255, blue: Double(hex & 0xFF) / 255)
    }
}

// MARK: - Sideways scrolling

/// Content that never wraps: it scrolls sideways, with the indicator flashed on appear and a
/// fade at the trailing edge while more lies to the right (app.md 4.11). `content` gets the
/// visible width, so rows can fill it.
struct SideScroll<Content: View>: View {
    @ViewBuilder var content: (CGFloat) -> Content
    @State private var viewWidth: CGFloat = 0
    @State private var contentWidth: CGFloat = 0
    @State private var offset: CGFloat = 0

    var body: some View {
        ScrollView(.horizontal) {
            content(viewWidth)
                .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { contentWidth = $0 }
        }
        .scrollIndicators(.visible)
        .scrollIndicatorsFlash(onAppear: true)
        .modifier(ScrollOffsetReader(offset: $offset))
        .onGeometryChange(for: CGFloat.self) { $0.size.width } action: { viewWidth = $0 }
        .overlay(alignment: .trailing) {
            if contentWidth - offset > viewWidth + 2 {
                FadeCue()
            }
        }
    }
}

/// The scroll offset where the system reports it (iOS 18, macOS 15); else it stays 0 and the
/// fade shows while the content is wider than the view.
private struct ScrollOffsetReader: ViewModifier {
    @Binding var offset: CGFloat

    func body(content: Content) -> some View {
        if #available(iOS 18, macOS 15, *) {
            content.onScrollGeometryChange(for: CGFloat.self) { $0.contentOffset.x } action: { _, x in offset = x }
        } else {
            content
        }
    }
}

/// The trailing fade with a small chevron: more to the right.
private struct FadeCue: View {
    @Environment(\.colorScheme) private var scheme

    var body: some View {
        ZStack(alignment: .trailing) {
            LinearGradient(colors: [.clear, scheme == .dark ? Color.black.opacity(0.55) : Color.white.opacity(0.85)],
                           startPoint: .leading, endPoint: .trailing)
                .frame(width: 28)
            Image(systemName: "chevron.compact.right")
                .font(.caption.weight(.bold))
                .foregroundStyle(.secondary)
                .padding(.trailing, 2)
        }
        .allowsHitTesting(false)
        .accessibilityHidden(true)
    }
}

// MARK: - Diffs

/// Diff lines, unified (app.md 4.11): a header per file with its counts, a gutter with line
/// numbers where known, no soft wrap.
struct DiffBody: View {
    let diff: ParsedDiff
    var maxLines: Int?
    @Environment(\.colorScheme) private var scheme
    @State private var showAll = false

    var body: some View {
        let p = DiffPalette(scheme)
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(diff.files.enumerated()), id: \.offset) { _, f in
                VStack(alignment: .leading, spacing: 0) {
                    header(f, p)
                    if f.binary {
                        Text("Binary file").font(.caption).foregroundStyle(.secondary).padding(8)
                    } else {
                        let ls = lines(f)
                        let cut = !showAll && maxLines.map { ls.count > $0 } == true
                        let shown = cut ? Array(ls.prefix(maxLines!)) : ls
                        let gutter = gutterWidth(ls)
                        SideScroll { width in
                            VStack(alignment: .leading, spacing: 0) {
                                ForEach(Array(shown.enumerated()), id: \.offset) { _, l in
                                    row(l, p, gutter: gutter, minWidth: width)
                                }
                            }
                            .fixedSize(horizontal: true, vertical: false)
                        }
                        if cut {
                            Button("Show all \(ls.count) lines") { showAll = true }
                                .font(.caption)
                                .padding(8)
                        }
                    }
                }
                .clipShape(RoundedRectangle(cornerRadius: 10))
                .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(.separator))
            }
        }
        .textSelection(.enabled)
    }

    private func header(_ f: ParsedDiff.File, _ p: DiffPalette) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(f.newPath ?? f.oldPath ?? "File")
                .font(.footnote.monospaced())
                .lineLimit(2)
                .truncationMode(.middle)
                .frame(maxWidth: .infinity, alignment: .leading)
            Text("+\(f.added)").font(.footnote.monospaced().weight(.semibold)).foregroundStyle(p.addedCount)
            Text("-\(f.removed)").font(.footnote.monospaced().weight(.semibold)).foregroundStyle(p.removedCount)
        }
        .padding(.horizontal, 10)
        .padding(.vertical, 6)
        .background(.quaternary.opacity(0.5))
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(f.newPath ?? f.oldPath ?? "File"), \(f.added) added, \(f.removed) removed")
    }

    private func lines(_ f: ParsedDiff.File) -> [ParsedDiff.Line] {
        var out: [ParsedDiff.Line] = []
        for h in f.hunks {
            if !h.header.isEmpty { out.append(.init(kind: .note, text: h.header, old: nil, new: nil)) }
            out.append(contentsOf: h.lines)
        }
        return out
    }

    private func gutterWidth(_ ls: [ParsedDiff.Line]) -> Int {
        let top = ls.compactMap { $0.new ?? $0.old }.max() ?? 0
        return top == 0 ? 0 : String(top).count
    }

    private func row(_ l: ParsedDiff.Line, _ p: DiffPalette, gutter: Int, minWidth: CGFloat) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 0) {
            if gutter > 0 {
                Text((l.new ?? l.old).map(String.init) ?? "")
                    .foregroundStyle(p.gutter)
                    .frame(minWidth: CGFloat(gutter) * 8 + 8, alignment: .trailing)
                    .padding(.trailing, 6)
                    .accessibilityHidden(true)
            }
            Text(prefix(l.kind) + l.text)
                .foregroundStyle(color(l.kind, p))
                .padding(.leading, 4)
                .padding(.trailing, 12)
        }
        .font(.caption.monospaced())
        .lineLimit(1)
        .fixedSize(horizontal: true, vertical: false)
        .frame(minWidth: minWidth, maxWidth: .infinity, alignment: .leading)
        .padding(.vertical, 1)
        .background(background(l.kind, p))
    }

    private func prefix(_ k: ParsedDiff.Line.Kind) -> String {
        switch k {
        case .added: return "+ "
        case .removed: return "- "
        case .context: return "  "
        case .note: return ""
        }
    }

    private func color(_ k: ParsedDiff.Line.Kind, _ p: DiffPalette) -> Color {
        switch k {
        case .added: return p.addedText
        case .removed: return p.removedText
        case .note: return p.gutter
        case .context: return .primary
        }
    }

    private func background(_ k: ParsedDiff.Line.Kind, _ p: DiffPalette) -> Color {
        switch k {
        case .added: return p.addedBackground
        case .removed: return p.removedBackground
        default: return .clear
        }
    }
}

// MARK: - Tool cards

/// A tool call card (app.md 4.1, 4.11): one sentence, folded by default. Open, it shows the
/// command, the diff, the input as labelled fields, the output and a subagent's messages.
struct ToolCardView: View {
    let card: ToolCard
    @State private var open = ToolCardView.openAtLaunch
    @State private var showAll = false
    @Environment(\.colorScheme) private var scheme
    @Environment(\.dynamicTypeSize) private var typeSize

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Button {
                withAnimation(.easeOut(duration: 0.15)) { open.toggle() }
            } label: {
                HStack(alignment: .center, spacing: 8) {
                    statusIcon
                    Text(sentence)
                        .lineLimit(typeSize.isAccessibilitySize ? nil : 3)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    if card.status == .error || card.status == .denied {
                        Text(statusWord)
                            .font(.caption)
                            .foregroundStyle(statusColor)
                    }
                    Image(systemName: open ? "chevron.down" : "chevron.right")
                        .font(.caption.weight(.semibold))
                        .foregroundStyle(.secondary)
                        .accessibilityHidden(true)
                }
                .padding(.horizontal, 12)
                .padding(.vertical, 10)
                .frame(minHeight: 44)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(card.sentence.text + (card.status == .done ? "" : ", " + statusWord))
            .accessibilityHint(open ? "Folds the details" : "Shows the details")
            if open {
                VStack(alignment: .leading, spacing: 10) {
                    detail
                    if !card.fields.isEmpty { fields }
                    if let o = card.output, !o.isEmpty { output(o) }
                    if !card.children.isEmpty {
                        VStack(alignment: .leading, spacing: 10) {
                            ForEach(card.children) { ChatItemView(item: $0) }
                        }
                        .padding(.leading, 8)
                        .overlay(alignment: .leading) { Rectangle().fill(.quaternary).frame(width: 2) }
                    }
                }
                .padding(.horizontal, 12)
                .padding(.bottom, 12)
            }
        }
        .background(.quaternary.opacity(0.3), in: RoundedRectangle(cornerRadius: 12))
        .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(.separator.opacity(0.6)))
    }

    /// Debug builds only: `-FlightdeckOpenToolCards YES` opens every tool card, for screenshots.
    static var openAtLaunch: Bool {
        #if DEBUG
        return UserDefaults.standard.bool(forKey: "FlightdeckOpenToolCards")
        #else
        return false
        #endif
    }

    /// The sentence: the verb in bold, the subject monospaced, the rest quieter, the counts in colour.
    private var sentence: AttributedString {
        let s = card.sentence
        let p = DiffPalette(scheme)
        var out = AttributedString(s.verb)
        out.font = .callout.weight(.semibold)
        if let subj = s.subject, !subj.isEmpty {
            var a = AttributedString(" " + subj)
            a.font = .callout.monospaced()
            out += a
        }
        if let r = s.rest, !r.isEmpty {
            var a = AttributedString(" " + r)
            a.font = .callout
            a.foregroundColor = .secondary
            out += a
        }
        if let n = s.added {
            var a = AttributedString(" +\(n)")
            a.font = .footnote.monospaced().weight(.semibold)
            a.foregroundColor = p.addedCount
            out += a
        }
        if let n = s.removed {
            var a = AttributedString(" -\(n)")
            a.font = .footnote.monospaced().weight(.semibold)
            a.foregroundColor = p.removedCount
            out += a
        }
        return out
    }

    @ViewBuilder private var detail: some View {
        switch card.detail {
        case .command(let c):
            SideScroll { width in
                Text(c)
                    .font(.callout.monospaced())
                    .fixedSize(horizontal: true, vertical: false)
                    .padding(8)
                    .frame(minWidth: width, alignment: .leading)
            }
            .background(.background, in: RoundedRectangle(cornerRadius: 8))
            .textSelection(.enabled)
            .accessibilityLabel("Command: \(c)")
        case .diff(let d):
            DiffBody(diff: d, maxLines: 60)
        case .input(let j):
            Text(j)
                .font(.caption.monospaced())
                .textSelection(.enabled)
        case .none:
            EmptyView()
        }
    }

    private var fields: some View {
        VStack(alignment: .leading, spacing: 6) {
            ForEach(Array(card.fields.enumerated()), id: \.offset) { _, f in
                VStack(alignment: .leading, spacing: 1) {
                    Text(f.label)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                    Text(f.value)
                        .font(f.code ? .footnote.monospaced() : .footnote)
                        .lineLimit(showAll ? nil : 6)
                        .textSelection(.enabled)
                }
                .accessibilityElement(children: .combine)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func output(_ o: String) -> some View {
        let lines = o.components(separatedBy: "\n")
        let limit = 12
        let shown = showAll || lines.count <= limit ? o : lines.prefix(limit).joined(separator: "\n")
        return VStack(alignment: .leading, spacing: 4) {
            Text("Output").font(.caption).foregroundStyle(.secondary)
            Text(shown)
                .font(.caption.monospaced())
                .foregroundStyle(.secondary)
                .textSelection(.enabled)
            if lines.count > limit && !showAll {
                Button("Show all \(lines.count) lines") { showAll = true }
                    .font(.caption)
            }
            if card.outputTruncated {
                Text("Only the first part of the output came through.")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
        }
    }

    private var statusWord: String {
        switch card.status {
        case .error: return "failed"
        case .denied: return "denied"
        case .running: return "running"
        default: return ChatRender.statusWord(card.status)
        }
    }

    private var statusColor: Color {
        let p = DiffPalette(scheme)
        switch card.status {
        case .error: return p.removedCount
        case .denied: return .orange
        default: return .secondary
        }
    }

    @ViewBuilder private var statusIcon: some View {
        let p = DiffPalette(scheme)
        switch card.status {
        case .running: ProgressView().controlSize(.mini)
        case .error: Image(systemName: "xmark.circle.fill").foregroundStyle(p.removedCount).font(.caption)
        case .denied: Image(systemName: "hand.raised.fill").foregroundStyle(.orange).font(.caption)
        default: Image(systemName: "checkmark").foregroundStyle(p.addedCount).font(.caption.weight(.bold))
        }
    }
}

// MARK: - Folded steps

/// Hidden steps folded into one line (app.md 4.11): "6 steps hidden: 3 reads, 2 edits, 1
/// command". A tap opens them in place.
struct FoldedStepsView: View {
    let summary: String
    let items: [ChatItem]
    @Binding var open: Bool
    var images: AttachmentImages?

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Button {
                withAnimation(.easeOut(duration: 0.15)) { open.toggle() }
            } label: {
                HStack(alignment: .firstTextBaseline, spacing: 8) {
                    Image(systemName: "eye.slash")
                        .foregroundStyle(.secondary)
                        .accessibilityHidden(true)
                    Text(summary)
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    Text(open ? "Hide" : "Show")
                        .font(.footnote.weight(.semibold))
                        .foregroundStyle(Color.accentColor)
                }
                .padding(.horizontal, 12)
                .padding(.vertical, 8)
                .frame(minHeight: 44)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .background(.quaternary.opacity(0.25), in: RoundedRectangle(cornerRadius: 12))
            .accessibilityLabel(summary)
            .accessibilityHint(open ? "Hides these steps again" : "Shows these steps here")
            if open {
                VStack(alignment: .leading, spacing: 12) {
                    ForEach(items) { ChatItemView(item: $0, images: images) }
                }
            }
        }
    }
}

// MARK: - Pinned banners

/// The session's open confirmations and the groups it coordinates, as one-line banners
/// (app.md 4.11). A tap opens the full card in a sheet sized to its content.
struct PinnedBanners: View {
    let target: SessionTarget
    @Environment(\.confirmations) private var confirms
    @Environment(\.groups) private var groups
    @State private var open: Pinned?

    enum Pinned: Identifiable, Hashable {
        case confirm(String)
        case group(String)
        var id: String {
            switch self {
            case .confirm(let id): return "c/" + id
            case .group(let id): return "g/" + id
            }
        }
    }

    private var confirmCards: [ConfirmCard] {
        (confirms?.cards(for: target) ?? []).filter { !$0.confirmation.isClosed }
    }

    private var groupCards: [GroupCard] {
        groups?.cards(coordinator: target) ?? []
    }

    var body: some View {
        let cs = confirmCards
        let gs = groupCards
        if !cs.isEmpty || !gs.isEmpty {
            TimelineView(.periodic(from: .now, by: 15)) { ctx in
                VStack(spacing: 6) {
                    ForEach(cs) { c in
                        banner(PinnedSummary.confirm(c.confirmation, remaining: confirms?.remaining(c, at: ctx.date)),
                               symbol: "questionmark.circle", attention: true) { open = .confirm(c.id) }
                    }
                    ForEach(gs) { g in
                        banner(PinnedSummary.group(g.group), symbol: "person.3", attention: false) { open = .group(g.id) }
                    }
                }
                .padding(.horizontal, 12)
                .padding(.top, 8)
                .padding(.bottom, 4)
                // The banners keep their height; the transcript gives way at large text sizes.
                .fixedSize(horizontal: false, vertical: true)
            }
            .sheet(item: $open) { p in
                PinnedSheet(pinned: p, close: { open = nil })
            }
            .onAppear {
                #if DEBUG
                // Debug builds only: `-FlightdeckShowPinned YES` opens the first banner at launch.
                if UserDefaults.standard.bool(forKey: "FlightdeckShowPinned"), open == nil {
                    open = cs.first.map { .confirm($0.id) } ?? gs.first.map { .group($0.id) }
                }
                #endif
            }
        }
    }

    private func banner(_ line: PinnedSummary.Line, symbol: String, attention: Bool, action: @escaping () -> Void) -> some View {
        Button(action: action) {
            HStack(alignment: .firstTextBaseline, spacing: 10) {
                Image(systemName: symbol)
                    .foregroundStyle(attention ? Color.orange : Color.secondary)
                    .accessibilityHidden(true)
                Text(bannerText(line))
                    .font(.subheadline)
                    .lineLimit(2)
                    .fixedSize(horizontal: false, vertical: true)
                    .frame(maxWidth: .infinity, alignment: .leading)
                Image(systemName: "chevron.up.chevron.down")
                    .font(.caption.weight(.semibold))
                    .foregroundStyle(.secondary)
                    .accessibilityHidden(true)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 10)
            .frame(minHeight: 44)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 12))
        .accessibilityLabel(line.text)
        .accessibilityHint("Opens the card")
    }

    private func bannerText(_ line: PinnedSummary.Line) -> AttributedString {
        var t = AttributedString(line.title)
        t.font = .subheadline.weight(.semibold)
        if let d = line.detail {
            var r = AttributedString(" · " + d)
            r.foregroundColor = .secondary
            t += r
        }
        return t
    }
}

/// One pinned card in a sheet sized to its content: a detent at the card's height, up to large.
struct PinnedSheet: View {
    let pinned: PinnedBanners.Pinned
    let close: () -> Void
    @Environment(\.confirmations) private var confirms
    @Environment(\.groups) private var groups
    @State private var height: CGFloat = 320

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Spacer()
                Button(action: close) {
                    Image(systemName: "xmark")
                        .font(.body.weight(.semibold))
                        .frame(width: 44, height: 44)
                }
                .buttonStyle(.plain)
                .foregroundStyle(.secondary)
                .accessibilityLabel("Close")
            }
            .padding(.horizontal, 8)
            ScrollView {
                card
                    .padding(.horizontal, 16)
                    .padding(.bottom, 16)
                    .onGeometryChange(for: CGFloat.self) { $0.size.height } action: { height = $0 + 52 }
            }
        }
        .presentationDetents([.height(min(max(height, 160), 2000)), .large])
        .presentationDragIndicator(.visible)
        #if os(macOS)
        .frame(minWidth: 460, minHeight: 300)
        #endif
    }

    @ViewBuilder private var card: some View {
        switch pinned {
        case .confirm(let id):
            if let center = confirms, let c = center.cards.first(where: { $0.id == id }) {
                ConfirmCardView(center: center, card: c)
            } else {
                Text("This card is gone.").foregroundStyle(.secondary)
            }
        case .group(let id):
            if let center = groups, let g = center.card(id) {
                TimelineView(.periodic(from: .now, by: 30)) { ctx in
                    GroupCardView(center: center, card: g, now: ctx.date)
                }
            } else {
                Text("This group is gone.").foregroundStyle(.secondary)
            }
        }
    }
}

/// The panel behind an image in the chat (app.md 4.11): dark grey in dark mode.
enum ImagePanel {
    static func fill(_ scheme: ColorScheme) -> Color {
        scheme == .dark ? Color(hex: 0x2C2C2E) : Color(hex: 0xF2F2F7)
    }
}
