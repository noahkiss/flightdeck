import CoreGraphics
import HabAPI
import PhotosUI
import SwiftUI
import UniformTypeIdentifiers
#if os(iOS)
import PencilKit
import UIKit
#else
import AppKit
#endif

// MARK: - Attachments on chat messages

/// The attachments of one user message: thumbnails from the hab's copies (app.md 4.4). Tap
/// opens an image full screen. A file that is no image shows as a chip.
struct AttachmentThumbRow: View {
    let ids: [String]
    let images: AttachmentImages?
    @State private var zoomed: ZoomTarget?

    struct ZoomTarget: Identifiable {
        var id: String
    }

    var body: some View {
        HStack(spacing: 6) {
            ForEach(ids, id: \.self) { id in
                thumb(id)
            }
        }
        #if os(iOS)
        .fullScreenCover(item: $zoomed) { t in AttachmentFullView(id: t.id, images: images) }
        #else
        .sheet(item: $zoomed) { t in AttachmentFullView(id: t.id, images: images).frame(minWidth: 640, minHeight: 480) }
        #endif
    }

    @ViewBuilder private func thumb(_ id: String) -> some View {
        if let img = images?.thumbnail(id) {
            Button { zoomed = ZoomTarget(id: id) } label: {
                Image(decorative: img, scale: 1).resizable().scaledToFill()
                    .frame(width: 96, height: 96)
                    .clipShape(RoundedRectangle(cornerRadius: 10))
            }
            .buttonStyle(.plain)
            .accessibilityLabel("Attached image")
            .accessibilityHint("Opens it full size")
        } else if images == nil || images?.didFail(id) == true {
            FileChipLabel(info: images?.file(id))
        } else {
            ProgressView()
                .frame(width: 96, height: 96)
                .task { await images?.load(id) }
        }
    }
}

/// A file attachment with no thumbnail: its name and size, or that the hab no longer has it.
struct FileChipLabel: View {
    let info: AttachmentImages.FileInfo?

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: info?.gone == true ? "doc.badge.ellipsis" : "doc")
            VStack(alignment: .leading, spacing: 1) {
                Text(info?.name ?? "File").lineLimit(1)
                if info?.gone == true {
                    Text("No longer on the hab").foregroundStyle(.secondary)
                } else if let size = info?.size {
                    Text(ByteCountFormatter.string(fromByteCount: Int64(size), countStyle: .file)).foregroundStyle(.secondary)
                }
            }
        }
        .font(.caption)
        .padding(8)
        .frame(maxWidth: 220, alignment: .leading)
        .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 10))
    }
}

/// One attachment full size, with pinch to zoom.
struct AttachmentFullView: View {
    let id: String
    let images: AttachmentImages?
    @Environment(\.dismiss) private var dismiss
    @State private var image: CGImage?
    @State private var failed = false
    @State private var scale: CGFloat = 1
    @State private var base: CGFloat = 1

    var body: some View {
        NavigationStack {
            GeometryReader { geo in
                ScrollView([.horizontal, .vertical]) {
                    Group {
                        if let image {
                            Image(decorative: image, scale: 1).resizable().scaledToFit()
                        } else if failed {
                            Text("The image did not load.").foregroundStyle(.secondary)
                        } else {
                            ProgressView()
                        }
                    }
                    .frame(width: geo.size.width * scale, height: geo.size.height * scale)
                }
                .gesture(MagnifyGesture()
                    .onChanged { scale = max(1, min(8, base * $0.magnification)) }
                    .onEnded { _ in base = scale })
            }
            .background(.black)
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("Done") { dismiss() }
                }
            }
            .task {
                image = await images?.full(id)
                failed = image == nil
            }
        }
    }
}

// MARK: - Composer

/// The attachments in the composer, above the text: thumbnails with a remove control. Tap an
/// image to mark it up (iPhone and iPad).
struct DraftStrip: View {
    @Binding var attachments: [DraftAttachment]
    @State private var marking: DraftAttachment?

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: 8) {
                ForEach(attachments) { a in
                    chip(a)
                }
            }
            .padding(.top, 6)
        }
        #if os(iOS)
        .fullScreenCover(item: $marking) { a in
            MarkupView(attachment: a) { marked in
                if let marked, let i = attachments.firstIndex(where: { $0.id == a.id }) {
                    attachments[i] = marked
                }
                marking = nil
            }
        }
        #endif
    }

    private func chip(_ a: DraftAttachment) -> some View {
        ZStack(alignment: .topTrailing) {
            Group {
                if let img = DraftThumbs.image(a) {
                    Button {
                        #if os(iOS)
                        marking = a
                        #endif
                    } label: {
                        Image(decorative: img, scale: 1).resizable().scaledToFill()
                            .frame(width: 64, height: 64)
                            .clipShape(RoundedRectangle(cornerRadius: 8))
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(a.name)
                    #if os(iOS)
                    .accessibilityHint("Opens markup")
                    #endif
                } else {
                    VStack(spacing: 2) {
                        Image(systemName: "doc")
                        Text(a.name).font(.caption2).lineLimit(2)
                    }
                    .frame(width: 64, height: 64)
                    .background(.quaternary.opacity(0.5), in: RoundedRectangle(cornerRadius: 8))
                }
            }
            Button {
                attachments.removeAll { $0.id == a.id }
            } label: {
                Image(systemName: "xmark.circle.fill")
                    .symbolRenderingMode(.palette)
                    .foregroundStyle(.white, .black.opacity(0.6))
            }
            .buttonStyle(.plain)
            .offset(x: 6, y: -6)
            .accessibilityLabel("Remove \(a.name)")
        }
    }
}

/// The attach button: Photos, Files, Paste (app.md 4.4).
struct AttachButton: View {
    @Binding var attachments: [DraftAttachment]
    var notice: (String) -> Void
    @State private var photos = false
    @State private var files = false
    @State private var picked: [PhotosPickerItem] = []

    var body: some View {
        Menu {
            Button { photos = true } label: { Label("Photos", systemImage: "photo") }
            Button { files = true } label: { Label("Files", systemImage: "folder") }
            Button(action: paste) { Label("Paste", systemImage: "doc.on.clipboard") }
        } label: {
            Image(systemName: "paperclip")
        }
        .menuIndicator(.hidden)
        .buttonStyle(.borderless)
        .fixedSize()
        .accessibilityLabel("Attach")
        .photosPicker(isPresented: $photos, selection: $picked, maxSelectionCount: 10, matching: .images)
        .onChange(of: picked) { _, items in
            guard !items.isEmpty else { return }
            picked = []
            Task {
                for item in items {
                    guard let data = try? await item.loadTransferable(type: Data.self) else { continue }
                    let ext = item.supportedContentTypes.first?.preferredFilenameExtension ?? "jpg"
                    if let a = AttachmentPrep.image(data, name: "photo.\(ext)") { add(a) }
                }
            }
        }
        .fileImporter(isPresented: $files, allowedContentTypes: [.item], allowsMultipleSelection: true) { result in
            guard case .success(let urls) = result else { return }
            for url in urls {
                let scoped = url.startAccessingSecurityScopedResource()
                defer { if scoped { url.stopAccessingSecurityScopedResource() } }
                if let size = (try? url.resourceValues(forKeys: [.fileSizeKey]))?.fileSize, size > AttachmentLimits.maxBytes {
                    notice("\(url.lastPathComponent) is over the 25 MB limit")
                    continue
                }
                guard let data = try? Data(contentsOf: url) else { continue }
                add(AttachmentPrep.file(data, name: url.lastPathComponent))
            }
        }
    }

    private func add(_ a: DraftAttachment) {
        if let r = AttachmentLimits.refusal(a) {
            notice(r)
        } else {
            attachments.append(a)
        }
    }

    private func paste() {
        #if os(iOS)
        let pb = UIPasteboard.general
        if let data = pb.data(forPasteboardType: UTType.png.identifier) ?? pb.image?.pngData(), let a = AttachmentPrep.image(data, name: "pasted.png") {
            add(a)
        } else {
            notice("The clipboard holds no image")
        }
        #else
        let pb = NSPasteboard.general
        if let data = pb.data(forType: .png) ?? pb.data(forType: .tiff), let a = AttachmentPrep.image(data, name: "pasted.png") {
            add(a)
        } else {
            notice("The clipboard holds no image")
        }
        #endif
    }
}

// MARK: - Markup

#if os(iOS)
/// Draws on an image before it is sent (app.md 4.4): PencilKit's pens, marker, eraser, lasso
/// and ruler over the image. Done draws the marks into the image the agent receives; Cancel
/// keeps the image as it was.
struct MarkupView: View {
    let attachment: DraftAttachment
    let done: (DraftAttachment?) -> Void
    @State private var canvas = PKCanvasView()

    var body: some View {
        NavigationStack {
            if let img = ImageDecode.thumbnail(attachment.data, maxPixel: AttachmentLimits.maxSide) {
                MarkupCanvas(image: img, canvas: canvas)
                    .ignoresSafeArea(edges: .bottom)
                    .toolbar {
                        ToolbarItem(placement: .cancellationAction) {
                            Button("Cancel") { done(nil) }
                        }
                        ToolbarItem(placement: .confirmationAction) {
                            Button("Done") { done(Markup.render(img, drawing: canvas.drawing, canvasSize: canvas.bounds.size, from: attachment)) }
                        }
                    }
                    .navigationTitle("Markup")
                    .navigationBarTitleDisplayMode(.inline)
            } else {
                Text("This image cannot be marked up.")
                    .toolbar { Button("Close") { done(nil) } }
            }
        }
    }
}

/// The image with a PencilKit canvas laid over exactly its fitted frame, and the tool picker.
struct MarkupCanvas: UIViewRepresentable {
    let image: CGImage
    let canvas: PKCanvasView

    func makeUIView(context: Context) -> MarkupHost {
        let host = MarkupHost(image: UIImage(cgImage: image), canvas: canvas)
        let picker = PKToolPicker()
        picker.setVisible(true, forFirstResponder: canvas)
        picker.addObserver(canvas)
        context.coordinator.picker = picker
        DispatchQueue.main.async { canvas.becomeFirstResponder() }
        return host
    }

    func updateUIView(_ v: MarkupHost, context: Context) {}

    func makeCoordinator() -> Coordinator { Coordinator() }

    final class Coordinator {
        var picker: PKToolPicker?
    }
}

/// Lays the canvas over the image's aspect-fit rectangle.
final class MarkupHost: UIView {
    let imageView: UIImageView
    let canvas: PKCanvasView

    init(image: UIImage, canvas: PKCanvasView) {
        imageView = UIImageView(image: image)
        imageView.contentMode = .scaleAspectFit
        self.canvas = canvas
        super.init(frame: .zero)
        backgroundColor = .black
        canvas.backgroundColor = .clear
        canvas.isOpaque = false
        canvas.drawingPolicy = .anyInput
        canvas.isScrollEnabled = false
        addSubview(imageView)
        addSubview(canvas)
    }

    required init?(coder: NSCoder) { nil }

    override func layoutSubviews() {
        super.layoutSubviews()
        imageView.frame = bounds
        canvas.frame = Markup.fit(imageView.image?.size ?? .zero, in: bounds)
    }
}
#endif

/// Markup geometry and rendering, kept apart from the views so it can be tested.
enum Markup {
    /// The aspect-fit rectangle of `size` centred in `bounds`.
    static func fit(_ size: CGSize, in bounds: CGRect) -> CGRect {
        guard size.width > 0, size.height > 0, bounds.width > 0, bounds.height > 0 else { return bounds }
        let s = min(bounds.width / size.width, bounds.height / size.height)
        let w = size.width * s, h = size.height * s
        return CGRect(x: bounds.midX - w / 2, y: bounds.midY - h / 2, width: w, height: h)
    }

    /// The image with `marks` (an image of the canvas, any size) drawn over it at full size,
    /// written as the original's type with no metadata.
    static func compose(_ base: CGImage, marks: CGImage?, png: Bool) -> Data? {
        guard let marks else { return AttachmentPrep.encode(base, png: png) }
        let w = base.width, h = base.height
        guard let ctx = CGContext(data: nil, width: w, height: h, bitsPerComponent: 8, bytesPerRow: 0,
                                  space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else { return nil }
        let rect = CGRect(x: 0, y: 0, width: w, height: h)
        ctx.draw(base, in: rect)
        ctx.draw(marks, in: rect)
        guard let out = ctx.makeImage() else { return nil }
        return AttachmentPrep.encode(out, png: png)
    }

    #if os(iOS)
    /// Draws the canvas's drawing into the image at the image's own pixel size.
    static func render(_ img: CGImage, drawing: PKDrawing, canvasSize: CGSize, from a: DraftAttachment) -> DraftAttachment? {
        guard canvasSize.width > 0, !drawing.strokes.isEmpty else { return a }
        let scale = CGFloat(img.width) / canvasSize.width
        let marks = drawing.image(from: CGRect(origin: .zero, size: canvasSize), scale: scale).cgImage
        let png = a.type == "image/png"
        guard let data = compose(img, marks: marks, png: png) else { return a }
        var out = a
        out.data = data
        return out
    }
    #endif
}
