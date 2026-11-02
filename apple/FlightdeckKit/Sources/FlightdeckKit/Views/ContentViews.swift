import HabAPI
import PDFKit
import SwiftUI
#if canImport(UIKit)
import UIKit
#else
import AppKit
#endif

// Rich content in the chat view (app.md 4.7): inline images with tap to
// zoom, document cards that expand, the modified-files list with diffs, and
// the side panel (a sheet on iPhone).

/// A plain placeholder for content the app cannot show now.
struct ContentPlaceholder: View {
    let text: String
    var symbol = "wifi.slash"

    var body: some View {
        Label(text, systemImage: symbol)
            .font(.callout)
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, minHeight: 60)
            .background(.quaternary.opacity(0.3), in: RoundedRectangle(cornerRadius: 8))
    }
}

/// Loads an item's bytes once per appearance, and again when the item or the hab's state changes.
/// The content closure gets the item as it was when its bytes were asked for: while a new
/// version loads, the old bytes show with the old version, so no cache files them under the new.
struct ContentLoader<Content: View>: View {
    let model: ContentModel
    let item: ContentItem
    @ViewBuilder let content: (ContentItem, Data, Bool) -> Content
    @State private var load: ContentBytes?
    @State private var loadedFor: ContentItem?

    var body: some View {
        Group {
            switch load {
            case nil:
                ProgressView().frame(maxWidth: .infinity, minHeight: 60)
            case .data(let d, let changed)?:
                content(loadedFor ?? item, d, changed)
            case .offline(let t)?:
                ContentPlaceholder(text: t)
            case .gone?:
                ContentPlaceholder(text: "File no longer on the host", symbol: "doc.questionmark")
            case .tooLarge(let n)?:
                ContentPlaceholder(text: "\(ContentText.size(n)): too large to show", symbol: "doc")
            case .failed(let e)?:
                ContentPlaceholder(text: e, symbol: "exclamationmark.triangle")
            }
        }
        .task(id: LoadKey(id: item.id, size: item.size, modified: item.modified, online: model.context.online)) {
            let asked = item
            let got = await model.bytes(for: asked)
            load = got
            loadedFor = ContentModel.version(asked, of: got)
        }
    }

    private struct LoadKey: Hashable {
        var id: String
        var size: Int?
        var modified: String?
        var online: Bool
    }
}

/// An image decoded once off the main actor, scaled to `maxPixel` on its long side.
struct DecodedImage<Content: View>: View {
    let model: ContentModel
    let item: ContentItem
    let data: Data
    let maxPixel: Int
    @ViewBuilder let content: (Image) -> Content
    @State private var image: CGImage?
    @State private var failed = false

    var body: some View {
        Group {
            if let image {
                content(Image(decorative: image, scale: 1))
            } else if failed {
                FileChip(item: item)
            } else {
                ProgressView().frame(maxWidth: .infinity, minHeight: 60)
            }
        }
        .task(id: "\(item.id)|\(data.count)|\(item.modified ?? "")|\(maxPixel)") {
            image = await model.image(for: item, data: data, maxPixel: maxPixel)
            failed = image == nil
        }
    }
}

/// Content shown inline under a chat row.
struct InlineContentView: View {
    let model: ContentModel
    let item: ContentItem

    var body: some View {
        if item.kind == .image {
            InlineImageView(model: model, item: item)
        } else {
            DocumentCard(model: model, item: item)
        }
    }
}

/// An image in the chat. Tap opens it full size with zoom.
struct InlineImageView: View {
    let model: ContentModel
    let item: ContentItem
    @State private var zoomed = false

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            ContentLoader(model: model, item: item) { version, data, _ in
                // About 3x the 360 pt frame: sharp on a phone, a fraction of a full decode.
                DecodedImage(model: model, item: version, data: data, maxPixel: 1080) { img in
                    Button { zoomed = true } label: {
                        img.resizable().scaledToFit()
                            .frame(maxWidth: 360, maxHeight: 260, alignment: .leading)
                            .clipShape(RoundedRectangle(cornerRadius: 8))
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel("Image \(item.title ?? item.name)")
                    .accessibilityHint("Opens it full size")
                }
                #if os(iOS)
                .fullScreenCover(isPresented: $zoomed) { ZoomedImage(model: model, item: version, data: data) }
                #else
                .sheet(isPresented: $zoomed) { ZoomedImage(model: model, item: version, data: data).frame(minWidth: 640, minHeight: 480) }
                #endif
            }
            Text(item.title ?? item.name)
                .font(.caption)
                .foregroundStyle(.secondary)
                .lineLimit(1)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// An image full size: pinch or scroll to zoom, double tap to fit or zoom in.
struct ZoomedImage: View {
    let model: ContentModel
    let item: ContentItem
    let data: Data
    private var title: String { item.title ?? item.name }
    @Environment(\.dismiss) private var dismiss
    @State private var scale: CGFloat = 1
    @State private var base: CGFloat = 1

    var body: some View {
        NavigationStack {
            GeometryReader { geo in
                ScrollView([.horizontal, .vertical]) {
                    DecodedImage(model: model, item: item, data: data, maxPixel: ZoomedImage.maxPixel) { image in
                        image.resizable().scaledToFit()
                    }
                    .frame(width: geo.size.width * scale, height: geo.size.height * scale)
                }
                .gesture(MagnifyGesture()
                    .onChanged { scale = max(1, min(8, base * $0.magnification)) }
                    .onEnded { _ in base = scale })
                .onTapGesture(count: 2) {
                    withAnimation { scale = scale > 1 ? 1 : 2.5 }
                    base = scale
                }
            }
            .background(.black)
            .navigationTitle(title)
            #if os(iOS)
            .navigationBarTitleDisplayMode(.inline)
            #endif
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
        }
    }
}

extension ZoomedImage {
    /// The zoomed view's decode bound: enough for 8x zoom of a phone-sized view.
    static let maxPixel = 4096
}

/// A file the app cannot render: name and size.
struct FileChip: View {
    let item: ContentItem

    var body: some View {
        Label {
            VStack(alignment: .leading, spacing: 1) {
                Text(item.title ?? item.name).lineLimit(1)
                if let s = item.size {
                    Text(ContentText.size(s))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }
        } icon: {
            Image(systemName: "doc")
        }
        .padding(8)
        .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 8))
    }
}

func kindSymbol(_ k: ContentKind) -> String {
    switch k {
    case .image: return "photo"
    case .markdown: return "doc.richtext"
    case .pdf: return "doc.text"
    case .html: return "globe"
    case .text: return "doc.plaintext"
    case .diff: return "plusminus"
    default: return "doc"
    }
}

func kindWord(_ k: ContentKind) -> String {
    switch k {
    case .markdown: return "Markdown"
    case .pdf: return "PDF"
    case .html: return "HTML"
    case .text: return "Text"
    case .diff: return "Diff"
    case .image: return "Image"
    default: return "File"
    }
}

/// A document in the chat: folded to its title and first lines; tap expands
/// it in place; a second control opens it full size.
struct DocumentCard: View {
    let model: ContentModel
    let item: ContentItem
    @State private var open = false
    @State private var full = false

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 6) {
                Button {
                    withAnimation(.easeOut(duration: 0.15)) { open.toggle() }
                } label: {
                    HStack(spacing: 6) {
                        Image(systemName: open ? "chevron.down" : "chevron.right")
                            .font(.caption2)
                            .foregroundStyle(.secondary)
                            .frame(width: 10)
                        Image(systemName: kindSymbol(item.kind))
                        VStack(alignment: .leading, spacing: 1) {
                            Text(item.title ?? item.name).font(.callout.weight(.medium)).lineLimit(1)
                            Text(kindWord(item.kind)).font(.caption).foregroundStyle(.secondary).lineLimit(1)
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityHint(open ? "Folds the document" : "Expands the document")
                Button("Open", systemImage: "arrow.up.left.and.arrow.down.right") { full = true }
                    .labelStyle(.iconOnly)
                    .buttonStyle(.borderless)
                    .accessibilityLabel("Open \(item.title ?? item.name) full size")
            }
            ContentLoader(model: model, item: item) { version, data, changed in
                VStack(alignment: .leading, spacing: 4) {
                    DocumentBody(item: version, data: data, folded: !open, model: model)
                    if changed {
                        Text("The file changed while loading.").font(.caption2).foregroundStyle(.tertiary)
                    }
                }
            }
        }
        .padding(10)
        .background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 10))
        .sheet(isPresented: $full) {
            NavigationStack {
                ScrollView { ContentDetail(model: model, item: item).padding() }
                    .navigationTitle(item.title ?? item.name)
                    .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { full = false } } }
            }
            #if os(macOS)
            .frame(minWidth: 640, minHeight: 520)
            #endif
        }
    }
}

/// The rendered document: Markdown, PDF, HTML as text, other text, or a file chip.
struct DocumentBody: View {
    let item: ContentItem
    let data: Data
    /// Folded shows the first lines only.
    var folded = false
    let model: ContentModel

    static let foldedLines = 8
    static let foldedBlocks = 4
    @State private var showAll = false

    var body: some View {
        switch item.kind {
        case .pdf:
            PDFDocumentView(data: data)
                .frame(height: folded ? 180 : 560)
        case .markdown:
            if showAll && !folded {
                fullText
            } else {
                // Folded keeps whole blocks, so a code fence is never cut open.
                let blocks = model.markdown(for: item, data: data)
                VStack(alignment: .leading, spacing: 4) {
                    MarkdownView(blocks: folded ? Array(blocks.prefix(Self.foldedBlocks)) : blocks)
                    if folded && blocks.count > Self.foldedBlocks {
                        Text("…").foregroundStyle(.secondary)
                    }
                    if !folded && ContentModel.markdownCut(data) {
                        // Only the first part is rendered; the whole file opens as text.
                        Divider()
                        Text("Showing the first \(Self.size(ContentText.previewLimit)) of \(Self.size(data.count))")
                            .font(.caption).foregroundStyle(.secondary)
                        Button("Show all as text") { showAll = true }
                            .font(.caption)
                    }
                }
            }
        case .diff:
            DiffBody(diff: model.parsedDiff(for: item, data: data), maxLines: folded ? Self.foldedLines : nil)
        case .image:
            DecodedImage(model: model, item: item, data: data, maxPixel: 2048) { $0.resizable().scaledToFit() }
        default:
            if data.prefix(4096).contains(0) {
                FileChip(item: item)
            } else if showAll && !folded {
                fullText
            } else {
                let preview = ContentText.preview(data, limit: folded ? 8 << 10 : ContentText.previewLimit)
                VStack(alignment: .leading, spacing: 4) {
                    Text(limitLines(preview.text, folded ? Self.foldedLines : nil))
                        .font(.caption.monospaced())
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    if preview.truncated && !folded {
                        Button("Show all \(Self.size(data.count))") { showAll = true }
                            .font(.caption)
                    }
                }
            }
        }
    }

    /// The whole file as a lazy list of line runs: one huge Text would stall the view.
    private var fullText: some View {
        LazyVStack(alignment: .leading, spacing: 0) {
            ForEach(Array(model.textChunks(for: item, data: data).enumerated()), id: \.offset) { _, run in
                Text(run).font(.caption.monospaced())
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
        .textSelection(.enabled)
    }

    static func size(_ n: Int) -> String { ContentText.size(n) }

    private func limitLines(_ s: String, _ n: Int?) -> String {
        guard let n else { return s }
        let lines = s.components(separatedBy: "\n")
        return lines.count <= n ? s : lines.prefix(n).joined(separator: "\n") + "\n…"
    }
}

/// A PDF through PDFKit.
#if canImport(UIKit)
struct PDFDocumentView: UIViewRepresentable {
    let data: Data
    func makeCoordinator() -> PDFSource { PDFSource() }
    func makeUIView(context: Context) -> PDFView {
        let v = PDFView()
        v.autoScales = true
        return v
    }
    func updateUIView(_ v: PDFView, context: Context) {
        context.coordinator.show(data, in: v)
    }
}
#else
struct PDFDocumentView: NSViewRepresentable {
    let data: Data
    func makeCoordinator() -> PDFSource { PDFSource() }
    func makeNSView(context: Context) -> PDFView {
        let v = PDFView()
        v.autoScales = true
        return v
    }
    func updateNSView(_ v: PDFView, context: Context) {
        context.coordinator.show(data, in: v)
    }
}
#endif

/// The bytes a PDF view shows. A parent redraw with the same bytes keeps the document, its
/// scroll position and zoom; re-serializing the document to compare would cost a full copy.
final class PDFSource {
    private(set) var shown: Data?
    private(set) var loads = 0

    /// Says whether the view needs a new document for these bytes.
    func needsLoad(_ data: Data) -> Bool { shown != data }

    func show(_ data: Data, in v: PDFView) {
        guard needsLoad(data) else { return }
        shown = data
        loads += 1
        v.document = PDFDocument(data: data)
    }
}

/// One item in full: an image, a document, or a modified file's diff.
struct ContentDetail: View {
    let model: ContentModel
    let item: ContentItem

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if item.change != nil && item.shown != true {
                DiffView(model: model, item: item)
            } else {
                ContentLoader(model: model, item: item) { version, data, _ in
                    DocumentBody(item: version, data: data, model: model)
                }
                if item.change != nil {
                    Divider()
                    DiffView(model: model, item: item)
                }
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// A modified file's diff, loaded from the hab.
struct DiffView: View {
    let model: ContentModel
    let item: ContentItem
    @State private var load: ContentDiffLoad?

    var body: some View {
        Group {
            switch load {
            case nil:
                ProgressView().frame(maxWidth: .infinity, minHeight: 60)
            case .diff(let d, let source, let truncated)?:
                VStack(alignment: .leading, spacing: 6) {
                    if d.files.isEmpty {
                        Text(source == "none" ? "No diff is known for this file." : "No changes.")
                            .font(.callout).foregroundStyle(.secondary)
                    } else {
                        DiffBody(diff: d)
                    }
                    if source == "transcript" {
                        Text("From the session's edit calls; line numbers are not known.").font(.caption2).foregroundStyle(.tertiary)
                    }
                    if truncated {
                        Text("The hab sent the first part of the diff.").font(.caption2).foregroundStyle(.tertiary)
                    }
                }
            case .offline(let t)?:
                ContentPlaceholder(text: t)
            case .gone?:
                ContentPlaceholder(text: "File no longer listed for this session", symbol: "doc.questionmark")
            case .failed(let e)?:
                ContentPlaceholder(text: e, symbol: "exclamationmark.triangle")
            }
        }
        .task(id: "\(item.id)-\(item.size ?? -1)-\(model.context.online)") {
            load = await model.diff(for: item)
        }
    }
}

/// Diff lines, unified, with line numbers where known.
struct DiffBody: View {
    let diff: ParsedDiff
    var maxLines: Int?

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(diff.files.enumerated()), id: \.offset) { _, f in
                VStack(alignment: .leading, spacing: 0) {
                    if diff.files.count > 1, let p = f.newPath ?? f.oldPath {
                        Text(p).font(.caption.weight(.semibold)).padding(.bottom, 2)
                    }
                    if f.binary {
                        Text("Binary file").font(.caption).foregroundStyle(.secondary)
                    }
                    ForEach(Array(lines(f).enumerated()), id: \.offset) { _, l in
                        row(l)
                    }
                }
            }
        }
        .textSelection(.enabled)
    }

    private func lines(_ f: ParsedDiff.File) -> [ParsedDiff.Line] {
        var out: [ParsedDiff.Line] = []
        for h in f.hunks {
            out.append(.init(kind: .note, text: h.header, old: nil, new: nil))
            out.append(contentsOf: h.lines)
        }
        if let n = maxLines, out.count > n { return Array(out.prefix(n)) + [.init(kind: .note, text: "…", old: nil, new: nil)] }
        return out
    }

    private func row(_ l: ParsedDiff.Line) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 6) {
            Text(l.new.map(String.init) ?? l.old.map(String.init) ?? "")
                .foregroundStyle(.tertiary)
                .frame(width: 34, alignment: .trailing)
            Text(prefix(l.kind) + l.text)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .font(.caption.monospaced())
        .foregroundStyle(l.kind == .note ? .secondary : .primary)
        .background(background(l.kind))
    }

    private func prefix(_ k: ParsedDiff.Line.Kind) -> String {
        switch k {
        case .added: return "+ "
        case .removed: return "- "
        case .context: return "  "
        case .note: return ""
        }
    }

    private func background(_ k: ParsedDiff.Line.Kind) -> Color {
        switch k {
        case .added: return .green.opacity(0.14)
        case .removed: return .red.opacity(0.14)
        default: return .clear
        }
    }
}

func changeWord(_ c: ContentChange?) -> String {
    switch c {
    case .added?: return "added"
    case .deleted?: return "deleted"
    case .renamed?: return "renamed"
    case .untracked?: return "new"
    case .modified?: return "modified"
    case let x?: return x.rawValue
    case nil: return ""
    }
}

func changeColor(_ c: ContentChange?) -> Color {
    switch c {
    case .added?, .untracked?: return .green
    case .deleted?: return .red
    default: return .orange
    }
}

/// The session's modified files as one folded card in the chat. It counts
/// the session's changes so far, never one tool call's (app.md 4.7).
struct ModifiedFilesCard: View {
    let model: ContentModel

    /// "1 file changed in this session": the card sits under the last row,
    /// which may be a call still waiting for approval, so it names its scope.
    static func title(_ n: Int) -> String {
        "\(n) file\(n == 1 ? "" : "s") changed in this session"
    }
    @State private var open = false
    @State private var selected: ContentItem?

    var body: some View {
        let files = model.modified
        if !files.isEmpty {
            VStack(alignment: .leading, spacing: 6) {
                Button {
                    withAnimation(.easeOut(duration: 0.15)) { open.toggle() }
                } label: {
                    HStack(spacing: 6) {
                        Image(systemName: open ? "chevron.down" : "chevron.right")
                            .font(.caption2).foregroundStyle(.secondary).frame(width: 10)
                        Image(systemName: "doc.badge.ellipsis")
                        Text(Self.title(files.count)).font(.callout.weight(.medium))
                        Spacer(minLength: 0)
                    }
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                if open {
                    ForEach(files, id: \.id) { f in
                        Button { selected = f } label: { ModifiedFileRow(item: f, folder: model.folder) }
                            .buttonStyle(.plain)
                    }
                }
            }
            .padding(10)
            .background(.quaternary.opacity(0.35), in: RoundedRectangle(cornerRadius: 10))
            .sheet(item: $selected) { f in
                NavigationStack {
                    ScrollView { DiffView(model: model, item: f).padding() }
                        .navigationTitle(f.name)
                        .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { selected = nil } } }
                }
                #if os(macOS)
                .frame(minWidth: 640, minHeight: 520)
                #endif
            }
        }
    }
}

extension ContentItem: @retroactive Identifiable {}

struct ModifiedFileRow: View {
    let item: ContentItem
    let folder: String?

    var body: some View {
        HStack(spacing: 6) {
            Text(changeWord(item.change))
                .font(.caption2.weight(.semibold))
                .foregroundStyle(changeColor(item.change))
                .frame(width: 58, alignment: .leading)
            Text(item.name).font(.callout).lineLimit(1)
            Text(relativeDir).font(.caption).foregroundStyle(.secondary).lineLimit(1).truncationMode(.head)
            Spacer(minLength: 0)
        }
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
    }

    private var relativeDir: String {
        let dir = (item.path as NSString).deletingLastPathComponent
        if let f = folder, dir.hasPrefix(f) { return String(dir.dropFirst(f.count)).trimmingCharacters(in: CharacterSet(charactersIn: "/")) }
        return (dir as NSString).abbreviatingWithTildeInPath
    }
}

/// A list row's identity: the section and the item.
struct Keyed: Identifiable {
    var section: String
    var item: ContentItem
    var id: String { section + "/" + item.id }
}

/// The session's content: documents, images and modified files. The side
/// panel on Mac and iPad, a sheet on iPhone.
public struct ContentPanel: View {
    let model: ContentModel
    @State private var selected: ContentItem?

    public init(model: ContentModel) {
        self.model = model
    }

    public var body: some View {
        NavigationStack {
            List {
                if let off = model.offlineText {
                    Label(off, systemImage: "wifi.slash").foregroundStyle(.secondary)
                } else if let p = model.problem {
                    Label(p, systemImage: "exclamationmark.triangle").foregroundStyle(.secondary)
                }
                section("Documents", model.documents)
                section("Images", model.images)
                if !model.modified.isEmpty {
                    Section("Modified files") {
                        // One item may sit in two sections: each section keys its rows apart.
                        ForEach(model.modified.map { Keyed(section: "modified", item: $0) }) { k in
                            NavigationLink(value: k.item) { ModifiedFileRow(item: k.item, folder: model.folder) }
                        }
                    }
                }
                if model.loaded && model.items.isEmpty && model.offlineText == nil {
                    Text("Nothing shown or changed yet").foregroundStyle(.secondary)
                }
            }
            .navigationTitle("Files")
            .navigationDestination(for: ContentItem.self) { it in
                ScrollView { ContentDetail(model: model, item: it).padding() }
                    .navigationTitle(it.title ?? it.name)
            }
            .refreshable { await model.refresh() }
        }
    }

    @ViewBuilder private func section(_ title: String, _ items: [ContentItem]) -> some View {
        if !items.isEmpty {
            Section(title) {
                ForEach(items.map { Keyed(section: title, item: $0) }) { k in
                    NavigationLink(value: k.item) {
                        Label(k.item.title ?? k.item.name, systemImage: kindSymbol(k.item.kind)).lineLimit(1)
                    }
                }
            }
        }
    }
}
