// swift-tools-version: 5.9
import PackageDescription

// Generated types for the gff-hab API and the module block vocabulary. The sources are generated
// from api/schema.json and api/modules/vocab.go by `make gen`; do not edit them by hand.
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
