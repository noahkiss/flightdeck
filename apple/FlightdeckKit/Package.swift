// swift-tools-version: 6.2
// FlightdeckKit: the platform-neutral half of the app. The macOS and iOS app
// targets (and any extension) link this one package. It builds on HabAPI, the
// generated gff-hab API types in swift/HabAPI, and on libghostty-spm for the
// terminal view.
import PackageDescription

// Swift 6 language mode on every target (the tools version sets it), plus
// the two Swift 6.2 approachable-concurrency features that Swift 6 mode does
// not turn on by itself. The package keeps nonisolated as its default
// isolation: it holds transport, SSH and audio code that must not run on the
// main actor, so UI types say @MainActor themselves.
let concurrency: [SwiftSetting] = [
    .enableUpcomingFeature("NonisolatedNonsendingByDefault"),
    .enableUpcomingFeature("InferIsolatedConformances"),
]

let package = Package(
    name: "FlightdeckKit",
    // English is the only language. The String Catalog in FlightdeckKit/Resources holds the
    // package's strings (app.md 22.3).
    defaultLocalization: "en",
    platforms: [.macOS(.v26), .iOS("26.1")],
    products: [
        .library(name: "FlightdeckKit", targets: ["FlightdeckKit"]),
        // The small part the extensions share with the app (push payload,
        // expiry text, Live Activity attributes and intent): Foundation,
        // ActivityKit and AppIntents only, so an extension stays light.
        .library(name: "FlightdeckShared", targets: ["FlightdeckShared"]),
        // On-device Parakeet for voice mode (voice.md): the Core ML runtime.
        // Only the iOS app links it, so the Mac app and the tests stay light.
        .library(name: "FlightdeckParakeet", targets: ["FlightdeckParakeet"]),
        // Quick entry for Capcom (app.md 3.18): the Mac's floating panel and its system-wide
        // shortcut. Only the Mac app links it.
        .library(name: "FlightdeckQuickEntry", targets: ["FlightdeckQuickEntry"]),
    ],
    dependencies: [
        .package(path: "../../swift/HabAPI"),
        // libghostty as a prebuilt XCFramework (GhosttyTerminal: views, input,
        // an in-memory backend). The version scheme is <major.minor>.<yyyymmdd>,
        // so pin it exactly and review the binary checksum on every bump.
        // 2.2.2026111701 is commit b9694b50, Ghostty 5ebe501a6342.
        .package(url: "https://github.com/Lakr233/libghostty-spm.git", exact: "2.2.2026112401"),
        // The in-app SSH carrier (transport.md 4.8). swift-nio-ssh is pre-1.0,
        // so pin it exactly; swift-nio follows semver.
        .package(url: "https://github.com/apple/swift-nio-ssh.git", exact: "0.15.2"),
        .package(url: "https://github.com/apple/swift-nio.git", from: "2.105.2"),
        // Parakeet TDT on Core ML, as the on-device spike measured it. Pre-1.0:
        // pin it exactly.
        .package(url: "https://github.com/FluidInference/FluidAudio.git", exact: "0.18.4"),
        // Markdown in the chat (app.md 4.6): native SwiftUI views over swift-markdown. Pinned
        // exactly; the LaTeX trait is off, so SwiftMath is not linked.
        .package(url: "https://github.com/LiYanan2004/MarkdownView.git", exact: "3.0.0", traits: []),
        // The parser MarkdownView uses, for the chat's own reads of a reply (its code blocks,
        // a fold by blocks). Pre-1.0: pinned exactly.
        .package(url: "https://github.com/swiftlang/swift-markdown.git", exact: "0.9.0"),
        // The quick entry panel's system-wide shortcut and its recorder (app.md 3.18): a Carbon
        // hot key, no Accessibility permission. macOS only. Pinned exactly.
        .package(url: "https://github.com/sindresorhus/KeyboardShortcuts.git", exact: "3.1.0"),
    ],
    targets: [
        .target(
            name: "FlightdeckShared",
            swiftSettings: concurrency
        ),
        .target(
            name: "FlightdeckKit",
            dependencies: [
                "FlightdeckShared",
                .product(name: "HabAPI", package: "HabAPI"),
                .product(name: "GhosttyTerminal", package: "libghostty-spm"),
                .product(name: "NIOSSH", package: "swift-nio-ssh"),
                .product(name: "NIOCore", package: "swift-nio"),
                .product(name: "NIOPosix", package: "swift-nio"),
                .product(name: "MarkdownView", package: "MarkdownView"),
                .product(name: "Markdown", package: "swift-markdown"),
            ],
            resources: [.process("Resources")],
            swiftSettings: concurrency
        ),
        .target(
            name: "FlightdeckParakeet",
            dependencies: [
                "FlightdeckKit",
                .product(name: "FluidAudio", package: "FluidAudio"),
            ],
            swiftSettings: concurrency
        ),
        .target(
            name: "FlightdeckQuickEntry",
            dependencies: [
                "FlightdeckKit",
                .product(name: "KeyboardShortcuts", package: "KeyboardShortcuts", condition: .when(platforms: [.macOS])),
            ],
            swiftSettings: concurrency
        ),
        .testTarget(
            name: "FlightdeckKitTests",
            dependencies: [
                "FlightdeckKit",
                "FlightdeckShared",
                .product(name: "NIOSSH", package: "swift-nio-ssh"),
                .product(name: "NIOCore", package: "swift-nio"),
                .product(name: "NIOPosix", package: "swift-nio"),
                .product(name: "Markdown", package: "swift-markdown"),
            ],
            exclude: ["Snapshots"],
            swiftSettings: concurrency
        ),
    ]
)
