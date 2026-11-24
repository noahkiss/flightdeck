import Foundation
import Observation

/// One menu command (app.md 3.13).
public enum MenuCommand: String, CaseIterable, Sendable {
    case newSession, newWindow, openInNewWindow, find
    case showChat, showTerminal, voice, stop, console
    case rename, restart, copyName, copyLink, close, markRead, markUnread
    case goSessions, goCapcom, goRounds, goSettings, nextNeedingYou, markAllRead
}

/// Where Go › Sessions, Capcom, Rounds and Settings take a window (app.md 3.13).
public enum GoTarget: String, CaseIterable, Sendable {
    case sessions, capcom, rounds, settings
}

/// The open session's controls, as its session view registers them with its window.
public struct SessionControls {
    public enum Mode: Sendable { case chat, terminal }

    public var target: SessionTarget
    /// The Chat | Terminal switch's value; nil when the view shows no switch.
    public var mode: Mode?
    public var setMode: (Mode) -> Void = { _ in }
    public var voiceAllowed = false
    public var voice: () -> Void = {}
    /// The agent works and the hab is online: Stop is on.
    public var working = false
    public var stop: () -> Void = {}
    public var hasConsole = false
    public var console: () -> Void = {}

    public init(target: SessionTarget) {
        self.target = target
    }
}

/// What a window offers the menus (app.md 3.13): the board's actions, its open session's row,
/// and the session view's controls. Each window holds one; it is also the window's token for
/// counting its own opens (app.md 3.12).
@MainActor
@Observable
public final class WindowCommands {
    public enum Kind: Sendable { case board, session }

    public let id = UUID()
    public let kind: Kind
    /// The window's open session row, by id; nil with none, or with the Capcom view open.
    public var openRowID: String?
    /// The open session view's controls.
    public var session: SessionControls?
    /// Go targets this window has: the four tabs on iPad, Sessions and Capcom on the Mac.
    public var goTargets: [GoTarget] = []
    @ObservationIgnored public var go: (GoTarget) -> Void = { _ in }
    @ObservationIgnored public var newSession: () -> Void = {}
    /// Raised by Find Sessions; the board's search field takes the cursor.
    public private(set) var finds = 0
    @ObservationIgnored public var openRow: (BoardRow) -> Void = { _ in }
    /// The window's questions for Rename and Close (app.md 3.11).
    var ask: SessionAsk?
    /// Opens a session in its own window; nil where the system allows one window only.
    @ObservationIgnored public var openWindow: ((SessionLink) -> Void)?

    /// Sheets up in this window, by name. While any is up, Stop is off, so ⌘. cancels the
    /// sheet instead (app.md 3.13).
    public private(set) var sheets: Set<String> = []

    public init(kind: Kind) {
        self.kind = kind
    }

    public func setSheet(_ name: String, _ up: Bool) {
        if up { sheets.insert(name) } else { sheets.remove(name) }
    }

    public func find() {
        finds += 1
    }
}

/// What the menus see of the focused window, as a value (app.md 3.13): a change re-reads the
/// menus. `enabled(_:)` is the rule.
public struct CommandState: Equatable, Sendable {
    public var board = false
    public var habsOnline = 0
    public var multipleWindows = false
    /// The open session's row, as the board lists it now.
    public var row: BoardRow?
    /// What the row offers (app.md 3.11); empty while a call on it runs.
    public var offers: [SessionAction] = []
    public var hasControls = false
    public var mode: SessionControls.Mode?
    public var voiceAllowed = false
    public var working = false
    public var hasConsole = false
    public var goTargets: [GoTarget] = []
    /// Another session needs the owner (app.md 3.16).
    public var nextNeedingYou = false
    public var unread = 0
    /// A sheet or alert is up in the window.
    public var sheetUp = false

    public init() {}

    public func enabled(_ c: MenuCommand) -> Bool {
        switch c {
        case .newSession: return board && habsOnline > 0
        case .newWindow: return multipleWindows
        case .openInNewWindow: return board && multipleWindows && row?.target.session != nil
        case .find: return board
        case .showChat: return hasControls && mode != nil && mode != .chat
        case .showTerminal: return hasControls && mode != nil && mode != .terminal
        case .voice: return hasControls && voiceAllowed
        case .stop: return hasControls && working && !sheetUp
        case .console: return hasControls && hasConsole
        case .rename: return offers.contains(.rename)
        case .restart: return offers.contains(.restart)
        case .copyName: return offers.contains(.copyName)
        case .copyLink: return row?.target.session != nil
        case .close: return offers.contains(.close)
        case .markRead: return row?.state == .done && row?.rank == .doneUnseen
        case .markUnread: return row?.state == .done && row?.rank == .atRest && row?.seenKey != nil
        case .goSessions: return goTargets.contains(.sessions)
        case .goCapcom: return goTargets.contains(.capcom)
        case .goRounds: return goTargets.contains(.rounds)
        case .goSettings: return goTargets.contains(.settings)
        case .nextNeedingYou: return board && nextNeedingYou
        case .markAllRead: return unread > 0
        }
    }
}

extension WindowCommands {
    /// The state the menus read, from the window and the fleet.
    public func state(board: Board, actions: SessionActionCenter, multipleWindows: Bool, sheetUp: Bool = false) -> CommandState {
        var s = CommandState()
        s.sheetUp = sheetUp || !sheets.isEmpty
        s.board = kind == .board
        s.habsOnline = board.onlineCount
        s.multipleWindows = multipleWindows
        let rows = board.rows
        if let id = openRowID, let r = rows.first(where: { $0.id == id }) {
            s.row = r
            s.offers = actions.running.contains(r.id) ? [] : actions.offers(r)
        }
        if let c = session {
            s.hasControls = true
            s.mode = c.mode
            s.voiceAllowed = c.voiceAllowed
            s.working = c.working
            s.hasConsole = c.hasConsole
        }
        s.goTargets = goTargets
        s.nextNeedingYou = NextNeedingYou.count(in: board.sections, excluding: openRowID) > 0
        s.unread = board.unreadCount
        return s
    }
}
