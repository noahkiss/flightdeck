import CoreGraphics
import SwiftUI

/// The fit rule of the terminal view on iPhone and iPad (app.md 5.4): a
/// readable font that follows Dynamic Type within bounds, and the grid that
/// fills the view at that font.
public enum TerminalFit {
    /// The font at the default text size (Large).
    public static let basePoints: CGFloat = 13
    /// The minimum readable size: fitted never draws smaller.
    public static let minPoints: CGFloat = 11
    /// The largest size, whatever the text size.
    public static let maxPoints: CGFloat = 24
    /// The font never grows so large that the view holds fewer columns.
    public static let minColumns = 40
    /// A cell's width over the point size, before libghostty has measured one.
    public static let defaultCellAspect: CGFloat = 0.6

    /// libghostty's font size for a size in points. On iOS libghostty draws
    /// its size a third larger than UIKit points (its 13 gives cells 10.5 pt
    /// wide, as a 17 pt monospace does); this converts, so the rule's sizes
    /// are the points the owner sees.
    public static func ghosttySize(_ points: CGFloat) -> CGFloat {
        points * 72 / 96
    }

    /// The body style's size at each Dynamic Type size, in points (Apple's
    /// type ramp). The terminal grows by the same factor as body text.
    static func bodyPoints(_ size: DynamicTypeSize) -> CGFloat {
        switch size {
        case .xSmall: 14
        case .small: 15
        case .medium: 16
        case .large: 17
        case .xLarge: 19
        case .xxLarge: 21
        case .xxxLarge: 23
        case .accessibility1: 28
        case .accessibility2: 33
        case .accessibility3: 40
        case .accessibility4: 47
        case .accessibility5: 53
        @unknown default: 17
        }
    }

    /// How much body text grows from Large at this text size.
    public static func textScale(_ size: DynamicTypeSize) -> CGFloat {
        bodyPoints(size) / bodyPoints(.large)
    }

    /// The font for a view `width` points wide at a text scale. `cellAspect`
    /// is a cell's width over its point size, as libghostty measured it.
    /// Rounded to half a point, so a small layout change does not reflow.
    public static func fontSize(width: CGFloat, textScale: CGFloat, cellAspect: CGFloat = defaultCellAspect) -> CGFloat {
        var f = basePoints * max(textScale, 0)
        f = min(f, maxPoints)
        let aspect = cellAspect > 0 ? cellAspect : defaultCellAspect
        if width > 0 {
            // Keep at least minColumns, unless that would go under the minimum.
            f = min(f, width / (CGFloat(minColumns) * aspect))
        }
        f = max(f, minPoints)
        return (f * 2).rounded(.down) / 2
    }

    /// The grid that fills `container` with cells of `cell` points, less half
    /// a cell so rounding never loses a column or row. At least 2 by 1.
    public static func grid(container: CGSize, cell: CGSize) -> (cols: Int, rows: Int)? {
        guard cell.width > 0, cell.height > 0, container.width > 0, container.height > 0 else { return nil }
        let c = Int(((container.width / cell.width) - PaneSizing.slack).rounded(.down))
        let r = Int(((container.height / cell.height) - PaneSizing.slack).rounded(.down))
        return (max(2, c), max(1, r))
    }
}
