// swift-tools-version: 6.0
// FlightdeckKit: the platform-neutral half of the app. The macOS and iOS app
// targets (and any extension) link this one package. It builds on HabAPI, the
// generated gff-hab API types in swift/HabAPI.
import PackageDescription

let package = Package(
    name: "FlightdeckKit",
    platforms: [.macOS(.v14), .iOS(.v17)],
    products: [
        .library(name: "FlightdeckKit", targets: ["FlightdeckKit"]),
    ],
    dependencies: [
        .package(path: "../../swift/HabAPI"),
    ],
    targets: [
        .target(
            name: "FlightdeckKit",
            dependencies: [.product(name: "HabAPI", package: "HabAPI")],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
        .testTarget(
            name: "FlightdeckKitTests",
            dependencies: ["FlightdeckKit"],
            swiftSettings: [.swiftLanguageMode(.v5)]
        ),
    ]
)
