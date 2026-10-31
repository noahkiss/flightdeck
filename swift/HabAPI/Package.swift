// swift-tools-version: 5.9
import PackageDescription

// Generated types for the gff-hab API. The sources are generated from api/schema.json by
// `make gen`; do not edit them by hand.
let package = Package(
    name: "HabAPI",
    platforms: [.macOS(.v14), .iOS(.v17)],
    products: [
        .library(name: "HabAPI", targets: ["HabAPI"]),
    ],
    targets: [
        .target(name: "HabAPI"),
        .testTarget(name: "HabAPITests", dependencies: ["HabAPI"]),
    ]
)
