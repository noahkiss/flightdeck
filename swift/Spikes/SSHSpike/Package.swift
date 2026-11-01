// swift-tools-version: 5.9
import PackageDescription

// Throwaway spike: swift-nio-ssh with a P-256 key (Secure Enclave or software) against an
// sshd, running `gff-hab connect` over exec channels. See README.md.
let package = Package(
    name: "SSHSpike",
    platforms: [.macOS(.v14)],
    dependencies: [
        .package(url: "https://github.com/apple/swift-nio-ssh", exact: "0.15.1"),
        .package(url: "https://github.com/apple/swift-nio", from: "2.65.0"),
    ],
    targets: [
        .executableTarget(
            name: "sshspike",
            dependencies: [
                .product(name: "NIOSSH", package: "swift-nio-ssh"),
                .product(name: "NIOCore", package: "swift-nio"),
                .product(name: "NIOPosix", package: "swift-nio"),
            ],
            path: "Sources/SSHSpike"
        ),
    ]
)
