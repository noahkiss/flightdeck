import Foundation
import Markdown

/// The chat's own reads of a reply's Markdown (app.md 4.6): its fenced and indented code blocks,
/// and a fold that keeps whole blocks. MarkdownView renders the text; this reads it with the same
/// parser (swift-markdown), so a code block here is a code block on screen.
public enum ChatMarkdown {
    /// The code of each code block, in order, without its fence.
    public static func codeBlocks(_ markdown: String) -> [String] {
        var walker = CodeBlocks()
        walker.visit(Document(parsing: markdown))
        return walker.found
    }

    /// The last code block's code: "Copy last code block" (app.md 4.12).
    public static func lastCodeBlock(_ markdown: String) -> String? {
        codeBlocks(markdown).last
    }

    /// The first `blocks` top-level blocks, as Markdown, and whether more followed. A fold keeps
    /// whole blocks, so a code fence is never cut open.
    public static func prefix(_ markdown: String, blocks: Int) -> (text: String, cut: Bool) {
        let doc = Document(parsing: markdown)
        let kids = Array(doc.blockChildren)
        guard kids.count > blocks else { return (markdown, false) }
        return (Document(kids.prefix(blocks)).format(), true)
    }

    /// The document with its raw HTML made inert (app.md 4.6): an HTML block becomes a code
    /// block and inline HTML a code span, both showing the markup as text. MarkdownView would
    /// render an HTML block in a web view and inline HTML through the system HTML importer, and
    /// agent text and files are not trusted.
    public static func withoutHTML(_ doc: Document) -> Document {
        guard containsHTML(doc) else { return doc }
        var r = HTMLAsText()
        return r.visit(doc) as? Document ?? Document()
    }

    /// Whether any HTML block or inline HTML is left.
    public static func containsHTML(_ doc: Document) -> Bool {
        var w = FindsHTML()
        w.visit(doc)
        return w.found
    }

    /// Whether the text has a fenced code block, by a scan with no parse: what the message menu
    /// needs to show Copy Last Code Block on a row that redraws on every streamed token.
    public static func mayHaveCodeBlock(_ markdown: String) -> Bool {
        markdown.contains("```") || markdown.contains("~~~")
    }

    /// Links in agent text open only web and mail addresses; any other scheme (this app's own,
    /// `file:`, `shortcuts:`) is dropped.
    public static func opensLink(_ url: URL) -> Bool {
        ["http", "https", "mailto"].contains(url.scheme?.lowercased() ?? "")
    }

    private struct HTMLAsText: MarkupRewriter {
        func visitHTMLBlock(_ html: HTMLBlock) -> (any Markdown.Markup)? {
            var code = html.rawHTML
            if code.hasSuffix("\n") { code.removeLast() }
            return CodeBlock(language: "html", code)
        }

        func visitInlineHTML(_ inlineHTML: InlineHTML) -> (any Markdown.Markup)? {
            InlineCode(inlineHTML.rawHTML)
        }
    }

    private struct FindsHTML: MarkupWalker {
        var found = false

        mutating func visitHTMLBlock(_ html: HTMLBlock) { found = true }
        mutating func visitInlineHTML(_ inlineHTML: InlineHTML) { found = true }
    }

    private struct CodeBlocks: MarkupWalker {
        var found: [String] = []

        mutating func visitCodeBlock(_ codeBlock: CodeBlock) {
            var code = codeBlock.code
            if code.hasSuffix("\n") { code.removeLast() }
            found.append(code)
        }
    }
}
