// swift-tools-version: 6.2
import PackageDescription

// Swift 6 language mode (the tools version sets it) with the approachable-concurrency features
// FlightdeckKit uses, so the two packages build under the same rules. The types are plain values;
// the default isolation stays nonisolated.
let concurrency: [SwiftSetting] = [
    .enableUpcomingFeature("NonisolatedNonsendingByDefault"),
    .enableUpcomingFeature("InferIsolatedConformances"),
]

// Generated types for the gff-hab API and the module block vocabulary. The sources are generated
// from api/schema.json and api/modules/vocab.go by `make gen`; do not edit them by hand.
let package = Package(
    name: "HabAPI",
    platforms: [.macOS(.v26), .iOS("26.1")],
    products: [
        .library(name: "HabAPI", targets: ["HabAPI"]),
    ],
    targets: [
        .target(name: "HabAPI", swiftSettings: concurrency),
        .testTarget(name: "HabAPITests", dependencies: ["HabAPI"], swiftSettings: concurrency),
    ]
)
