//
//  FlightdeckTabKeys.swift
//  FlightDeck
//
//  Ctrl-Tab and Ctrl-Shift-Tab switch to the next and previous terminal tab
//  on the Mac, as in Ghostty. Without them, the UIKit focus system takes
//  Ctrl-Tab to move between focus groups.
//

#if targetEnvironment(macCatalyst)
import UIKit

extension FlightdeckHooks {
    /// `CatalystAppDelegate.buildMenu`, after upstream's menus are built.
    static func buildMainMenu(_ builder: UIMenuBuilder) {
        let next = UIKeyCommand(
            title: String(localized: "Next Tab"),
            action: #selector(UIApplication.ghostty_nextTab(_:)),
            input: "\t",
            modifierFlags: [.control]
        )
        let previous = UIKeyCommand(
            title: String(localized: "Previous Tab"),
            action: #selector(UIApplication.ghostty_previousTab(_:)),
            input: "\t",
            modifierFlags: [.control, .shift]
        )
        for command in [next, previous] {
            // Upstream's Tabs menu already lists these actions under Cmd-{ and
            // Cmd-}; these chords work without a second visible item.
            command.attributes = .hidden
            command.wantsPriorityOverSystemBehavior = true
        }
        let menu = UIMenu(
            title: "",
            identifier: UIMenu.Identifier("dev.flightdeck.tabs.control-tab"),
            options: .displayInline,
            children: [next, previous]
        )
        let tabs = UIMenu.Identifier("com.rootshell.tabs")
        if builder.menu(for: tabs) != nil {
            builder.insertChild(menu, atEndOfMenu: tabs)
        } else {
            builder.insertChild(menu, atEndOfMenu: .window)
        }
    }
}
#endif
