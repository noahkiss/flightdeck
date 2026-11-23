// swift-tools-version: 6.2
import PackageDescription

// Generated types for the gff-hab API and the module block vocabulary. The sources are generated
// from api/schema.json and api/modules/vocab.go by `make gen`; do not edit them by hand.
let package = Package(
    name: "HabAPI",
    platforms: [.macOS(.v26), .iOS("26.1")],
    products: [
        .library(name: "HabAPI", targets: ["HabAPI"]),
    ],
    targets: [
        .target(name: "HabAPI", swiftSettings: [.swiftLanguageMode(.v5)]),
        .testTarget(name: "HabAPITests", dependencies: ["HabAPI"], swiftSettings: [.swiftLanguageMode(.v5)]),
    ]
)
