/// A session window's customisable toolbar on the Mac (app.md 3.13, 3.14). Capcom windows and
/// agent windows each have their own toolbar id, so the arrangement the system keeps for one
/// kind never drops the other kind's items. Each kind's items are fixed: a control that does
/// not apply now is off, never left out, since the system adds no item to an arrangement the
/// owner already changed.
public enum SessionToolbar {
    public enum Item: String, CaseIterable, Sendable {
        case view, newConversation = "new-conversation", voice, console, showInChat = "show-in-chat", files, friction
    }

    /// The toolbar id of a Capcom window or an agent window.
    public static func id(capcom: Bool) -> String {
        capcom ? "session.capcom" : "session.agent"
    }

    /// The items of a window kind, in their first order.
    public static func items(capcom: Bool) -> [Item] {
        capcom ? Item.allCases : Item.allCases.filter { $0 != .view && $0 != .newConversation }
    }

    /// Whether an item's control is on for this session: `session` says the target names a
    /// session, `show` that Show in chat has a filter to offer. Voice turns itself off.
    public static func enabled(_ item: Item, context: ChatContext, session: Bool, show: Bool) -> Bool {
        switch item {
        case .view: !context.isCapcomService
        case .newConversation, .voice, .files: true
        case .console: session && !context.isConsole && !context.isCapcomService
        case .showInChat: show && !context.isCapcomService
        case .friction: context.takesFriction
        }
    }
}
