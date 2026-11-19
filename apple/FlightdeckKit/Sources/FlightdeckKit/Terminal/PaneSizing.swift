import CoreGraphics

/// How the terminal view sizes its pane (app.md 5.4).
public enum TerminalSizeMode: String, Sendable, Equatable {
    /// The grid that fills the view at a readable font: the default on iOS.
    case fitted
    /// The size another client, or nobody, last gave the pane.
    case host
}

/// The size rule of the terminal view (app.md 5.1, 5.4, hab-logic-api.md 6).
///
/// The terminal's grid must equal the PTY's size, or output wraps wrongly. So
/// the surface is always laid out at the pane's size in cells, then scaled to
/// fit the space the view has. The mode and the space decide the size this
/// client wants; that size goes to the hab only while this client owns the
/// pane (and once when Host size is chosen).
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
    public var mode: TerminalSizeMode = .fitted
    /// The pane's size as another client or nobody last set it: what Host
    /// size keeps. Nil until the pane's size is known.
    public var hostSize: GridSize?

    public init() {}

    /// Extra space past the last cell, in cells, so rounding never loses a
    /// column or row.
    static let slack: CGFloat = 0.5

    public var isOwner: Bool { owner != nil && owner == me }
    /// Another client owns the pane.
    public var ownedByOther: Bool { owner != nil && owner != me }

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
        TerminalFit.grid(container: container, cell: cell) ?? (cols, rows)
    }

    /// The size this client wants for the pane in its mode.
    public var want: (cols: Int, rows: Int) {
        switch mode {
        case .fitted: return desired
        case .host: return hostSize.map { ($0.cols, $0.rows) } ?? (cols, rows)
        }
    }

    /// Whether the container holds a usable size.
    public var hasContainer: Bool { container.width >= cell.width * 4 && container.height >= cell.height * 2 }

    /// What the size control shows (app.md 5.4).
    public var label: SizeLabel {
        let g = GridSize(cols: cols, rows: rows)
        if ownedByOther { return .otherDevice(g) }
        let w = want
        if mode == .fitted, isOwner || (w.cols == cols && w.rows == rows) { return .fitted(g) }
        return .host(g)
    }
}

/// A pane size in cells.
public struct GridSize: Sendable, Equatable, Hashable {
    public var cols: Int
    public var rows: Int
    public init(cols: Int, rows: Int) {
        self.cols = cols
        self.rows = rows
    }

    /// "50 × 38".
    public var text: String { "\(cols) × \(rows)" }
    /// "50 columns by 38 rows", for VoiceOver.
    public var spoken: String { "\(cols) columns by \(rows) rows" }
}

/// The size control's text (app.md 5.4).
public enum SizeLabel: Sendable, Equatable {
    case fitted(GridSize)
    case host(GridSize)
    /// Another client owns the pane and sets its size.
    case otherDevice(GridSize)

    public var size: GridSize {
        switch self {
        case .fitted(let g), .host(let g), .otherDevice(let g): g
        }
    }

    public var modeName: String {
        switch self {
        case .fitted: "Fitted"
        case .host, .otherDevice: "Host size"
        }
    }

    /// "Fitted · 50 × 38".
    public var text: String { "\(modeName) · \(size.text)" }

    /// A second line, when there is something to explain.
    public var note: String? {
        if case .otherDevice = self { return "Another device sets this size." }
        return nil
    }

    public var spoken: String {
        var s = "Terminal size: \(modeName), \(size.spoken)"
        if let n = note { s += ". " + n }
        return s
    }
}
