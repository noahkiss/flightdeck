import CoreGraphics

/// The size rule of the terminal view (app.md 5.1, hab-logic-api.md 6).
///
/// The terminal's grid must equal the PTY's size, or output wraps wrongly. So
/// the surface is always laid out at the pane's size in cells, then scaled to
/// fit the space the view has. The space only decides the size this client
/// wants; that size goes to the hab only while this client owns the pane.
public struct PaneSizing: Sendable, Equatable {
    public var cols = 80
    public var rows = 24
    public var owner: String?
    /// This client's id on the hab; nil before the welcome.
    public var me: String?
    /// One cell, in points.
    public var cell = CGSize(width: 7.8, height: 16)
    /// The space the view has, in points.
    public var container = CGSize.zero

    public init() {}

    /// Extra space past the last cell, in cells, so rounding never loses a
    /// column or row.
    static let slack: CGFloat = 0.5

    public var isOwner: Bool { owner != nil && owner == me }

    /// The surface's size in points: the pane's grid plus the slack.
    public var surface: CGSize {
        CGSize(width: (CGFloat(cols) + Self.slack) * cell.width, height: (CGFloat(rows) + Self.slack) * cell.height)
    }

    /// Scale that fits the surface in the container; never above 1.
    public var scale: CGFloat {
        guard container.width > 0, container.height > 0 else { return 1 }
        let s = surface
        return min(1, container.width / s.width, container.height / s.height)
    }

    /// The grid that fills the container at the current cell size.
    public var desired: (cols: Int, rows: Int) {
        guard cell.width > 0, cell.height > 0 else { return (cols, rows) }
        let c = Int(((container.width / cell.width) - Self.slack).rounded(.down))
        let r = Int(((container.height / cell.height) - Self.slack).rounded(.down))
        return (max(2, c), max(1, r))
    }

    /// Whether the container holds a usable size.
    public var hasContainer: Bool { container.width >= cell.width * 4 && container.height >= cell.height * 2 }
}
