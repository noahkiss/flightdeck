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

    private struct CodeBlocks: MarkupWalker {
        var found: [String] = []

        mutating func visitCodeBlock(_ codeBlock: CodeBlock) {
            var code = codeBlock.code
            if code.hasSuffix("\n") { code.removeLast() }
            found.append(code)
        }
    }
}
