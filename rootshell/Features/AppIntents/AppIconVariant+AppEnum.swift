//
//  AppIconVariant+AppEnum.swift
//  rootshell
//
//  Exposes AppIconManager.AppIconVariant to Shortcuts as an AppEnum.
//

import AppIntents

#if !FLIGHTDECK // FlightDeck's conformance: rootshell/Flightdeck/Icons/AppIconVariant+Flightdeck.swift
extension AppIconManager.AppIconVariant: AppEnum {
    nonisolated static var typeDisplayRepresentation: TypeDisplayRepresentation {
        TypeDisplayRepresentation(name: "App Icon")
    }

    /// `appintentsmetadataprocessor` parses this dictionary at build time and
    /// requires an exhaustive literal — it can't evaluate runtime expressions.
    /// English literals match `AppIconVariant.displayName`'s switch so existing
    /// `Localizable.xcstrings` translations apply to both call sites.
    nonisolated static var caseDisplayRepresentations: [AppIconManager.AppIconVariant: DisplayRepresentation] {
        [
            .defaultIcon:           "Blue",
            .black:                 "Black",
            .crt:                   "CRT",
            .noBorder:              "Blue - No Border",
            .underscore:            "Underscore",
            .sixColors:             "Six Colors",
            .sixColorsDark:         "Six Colors Dark",
            .original:              "Original",
            .radicalSolarizedDark:  "Solarized Dark",
            .radicalSolarizedLight: "Solarized Light",
            .radicalDracula:        "Dracula",
            .radicalNord:           "Nord",
            .radicalGruvboxDark:    "Gruvbox Dark",
            .radicalTokyoNight:     "Tokyo Night",
            .radicalCatppuccin:     "Catppuccin",
            .radicalBases:          "Bases",
            .radicalMonoLight:      "Mono Light",
            .radicalMonokai:        "Monokai",
            .radicalMonoDark:       "Mono Dark",
            .radicalRosePine:       "Rose Pine",
        ]
    }
}
#endif
