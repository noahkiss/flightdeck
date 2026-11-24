import HabAPI
import SwiftUI

/// A session state's shape, for Differentiate Without Color (app.md 22.2): with the setting on,
/// the state dot becomes a symbol, so no state is told by colour alone.
public enum StateDotShape: String, CaseIterable, Sendable {
    /// Blocked: the agent waits for the owner.
    case waiting
    /// The turn stopped with an error.
    case error
    /// Done, not yet seen.
    case doneUnseen
    /// Done and seen.
    case done
    /// Working.
    case working
    /// Idle.
    case idle
    /// No agent state: a shell, or not known.
    case other

    /// The SF Symbol drawn in place of the dot. Each state has its own outline, not only a
    /// different fill.
    public var symbol: String {
        switch self {
        case .waiting: "exclamationmark.triangle.fill"
        case .error: "exclamationmark.octagon.fill"
        case .doneUnseen: "checkmark.circle.fill"
        case .done: "checkmark"
        case .working: "ellipsis.circle.fill"
        case .idle: "circle"
        case .other: "circle.dashed"
        }
    }

    /// The shape for an agent state, as `BoardRow.stateColor` colours it.
    public static func of(_ state: AgentStatus?, rank: BoardRank) -> StateDotShape {
        switch state {
        case .blocked?: .waiting
        case .error?: .error
        case .done?: rank == .doneUnseen ? .doneUnseen : .done
        case .working?: .working
        case .idle?: .idle
        default: .other
        }
    }
}

extension BoardRow {
    var dotShape: StateDotShape { StateDotShape.of(state, rank: rank) }
}

/// The state dot: a filled circle in the state's colour, or the state's symbol under
/// Differentiate Without Color (app.md 22.2).
struct StateDot: View {
    let shape: StateDotShape
    let color: Color
    var size: CGFloat = 10
    @Environment(\.accessibilityDifferentiateWithoutColor) private var differentiate
    /// The symbol grows with Dynamic Type, as the text beside it does.
    @ScaledMetric(relativeTo: .subheadline) private var scale: CGFloat = 1

    init(shape: StateDotShape, color: Color, size: CGFloat = 10) {
        self.shape = shape
        self.color = color
        self.size = size
    }

    init(_ row: BoardRow, size: CGFloat = 10) {
        self.init(shape: row.dotShape, color: row.stateColor, size: size)
    }

    var body: some View {
        if differentiate {
            Image(systemName: shape.symbol)
                .resizable()
                .scaledToFit()
                .fontWeight(.bold)
                .foregroundStyle(color)
                .frame(width: (size + 4) * scale, height: (size + 4) * scale)
        } else {
            Circle().fill(color).frame(width: size, height: size)
        }
    }
}
