// swift-tools-version: 6.0
import PackageDescription
let package = Package(
    name: "MacCluster",
    platforms: [.macOS(.v14)],
    products: [.executable(name: "MacCluster", targets: ["MacCluster"])],
    dependencies: [.package(url: "https://github.com/migueldeicaza/SwiftTerm.git", exact: "1.11.2")],
    targets: [
        .executableTarget(name: "MacCluster", dependencies: [.product(name: "SwiftTerm", package: "SwiftTerm")]),
        .testTarget(name: "MacClusterTests", dependencies: ["MacCluster"])
    ]
)
