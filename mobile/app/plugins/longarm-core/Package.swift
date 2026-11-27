// swift-tools-version: 5.9
// The LongarmCore Capacitor plugin: the Go core (Longarm.xcframework,
// built by `gomobile bind -target=ios` into ios/, docs/mobile.md) and the
// plugin that starts it.
import PackageDescription

let package = Package(
    name: "LongarmCore",
    platforms: [.iOS(.v15)],
    products: [
        .library(name: "LongarmCore", targets: ["LongarmCorePlugin"])
    ],
    dependencies: [
        .package(url: "https://github.com/ionic-team/capacitor-swift-pm.git", from: "8.0.0")
    ],
    targets: [
        .binaryTarget(name: "Longarm", path: "ios/Longarm.xcframework"),
        .target(
            name: "LongarmCorePlugin",
            dependencies: [
                .product(name: "Capacitor", package: "capacitor-swift-pm"),
                "Longarm",
            ],
            path: "ios/Sources/LongarmCorePlugin")
    ]
)
