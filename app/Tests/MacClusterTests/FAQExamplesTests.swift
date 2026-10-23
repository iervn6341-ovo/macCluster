import XCTest
@testable import MacCluster

final class FAQExamplesTests: XCTestCase {
    private var root: URL { URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent() }

    private func file(_ name: String) throws -> String {
        try String(contentsOf: root.appendingPathComponent("examples/" + name), encoding: .utf8).trimmingCharacters(in: .newlines)
    }

    func testFAQShowsTheValidatedExampleFilesVerbatim() throws {
        XCTAssertEqual(FAQExamples.docker, try file("faq-docker.yaml"))
        XCTAssertEqual(FAQExamples.dockerVolumes, try file("faq-docker-volumes.yaml"))
        XCTAssertEqual(FAQExamples.native, try file("faq-native.yaml"))
        XCTAssertEqual(FAQExamples.nativeEnvironment, try file("faq-native-environment.yaml"))
        XCTAssertEqual(FAQExamples.nativeFixedPorts, try file("faq-native-fixed-ports.yaml"))
    }
}
