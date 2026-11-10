// swift-tools-version: 6.0
// FlightdeckKit: the platform-neutral half of the app. The macOS and iOS app
// targets (and any extension) link this one package. It builds on HabAPI, the
// generated gff-hab API types in swift/HabAPI, and on libghostty-spm for the
// terminal view.
import PackageDescription

let package = Package(
    name: "FlightdeckKit",
    platforms: [.macOS(.v14), .iOS(.v17)],
    products: [
        .library(name: "FlightdeckKit", targets: ["FlightdeckKit"]),
        // The small part the extensions share with the app (push payload,
        // expiry text, Live Activity attributes and intent): Foundation,
        // ActivityKit and AppIntents only, so an extension stays light.
        .library(name: "FlightdeckShared", targets: ["FlightdeckShared"]),
        // On-device Parakeet for voice mode (voice.md): the Core ML runtime.
        // Only the iOS app links it, so the Mac app and the tests stay light.
        .library(name: "FlightdeckParakeet", targets: ["FlightdeckParakeet"]),
    ],
    dependencies: [
        .package(path: "../../swift/HabAPI"),
        // libghostty as a prebuilt XCFramework (GhosttyTerminal: views, input,
        // an in-memory backend). The version scheme is <major.minor>.<yyyymmdd>,
        // so pin it exactly and review the binary checksum on every bump.
        // 1.6.20260909 is commit 7e45d271, Ghostty 82938b633ba6.
        .package(url: "https://github.com/Lakr233/libghostty-spm.git", exact: "2.2.2026110702"),
        // The in-app SSH carrier (transport.md 4.8). swift-nio-ssh is pre-1.0,
        // so pin it exactly; swift-nio follows semver.
        .package(url: "https://github.com/apple/swift-nio-ssh.git", exact: "0.15.1"),
        .package(url: "https://github.com/apple/swift-nio.git", from: "2.104.0"),
        // Parakeet TDT on Core ML, as the on-device spike measured it. Pre-1.0:
        // pin it exactly.
        .package(url: "https://github.com/FluidInference/FluidAudio.git", exact: "0.17.7"),
    ],
    targets: [
        .target(
            name: "FlightdeckShared",
            swiftSettings: [.swiftLanguageMode(.v5)]
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
            ],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
        .target(
            name: "FlightdeckParakeet",
            dependencies: [
                "FlightdeckKit",
                .product(name: "FluidAudio", package: "FluidAudio"),
            ],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
        .testTarget(
            name: "FlightdeckKitTests",
            dependencies: [
                "FlightdeckKit",
                "FlightdeckShared",
                .product(name: "NIOSSH", package: "swift-nio-ssh"),
                .product(name: "NIOCore", package: "swift-nio"),
                .product(name: "NIOPosix", package: "swift-nio"),
            ],
            exclude: ["Snapshots"],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
    ]
)
