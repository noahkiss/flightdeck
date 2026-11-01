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
    ],
    dependencies: [
        .package(path: "../../swift/HabAPI"),
        // libghostty as a prebuilt XCFramework (GhosttyTerminal: views, input,
        // an in-memory backend). The version scheme is <major.minor>.<yyyymmdd>,
        // so pin it exactly and review the binary checksum on every bump.
        // 1.6.20260909 is commit 7e45d271, Ghostty 82938b633ba6.
        .package(url: "https://github.com/Lakr233/libghostty-spm.git", exact: "1.6.20260909"),
    ],
    targets: [
        .target(
            name: "FlightdeckKit",
            dependencies: [
                .product(name: "HabAPI", package: "HabAPI"),
                .product(name: "GhosttyTerminal", package: "libghostty-spm"),
            ],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
        .testTarget(
            name: "FlightdeckKitTests",
            dependencies: ["FlightdeckKit"],
            exclude: ["Snapshots"],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
    ]
)
