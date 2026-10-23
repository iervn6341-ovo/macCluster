import XCTest
@testable import MacCluster

final class StatusStyleTests: XCTestCase {
    func testEveryPhaseTheBackendProducesHasADeliberateTone() {
        let expected: [String: StatusTone] = [
            "Running": .running, "Ready": .running,
            "Pending": .transitioning, "Joining": .transitioning, "Leaving": .transitioning,
            "Restarting": .transitioning, "Paused": .transitioning, "Removing": .transitioning,
            "Succeeded": .stopped, "Stopped": .stopped, "Created": .stopped,
            "Failed": .failed, "Error": .failed, "Dead": .failed, "Removed": .failed,
            "Unknown": .unknown,
        ]
        for (phase, tone) in expected { XCTAssertEqual(StatusStyle.tone(phase: phase), tone, phase) }
        XCTAssertEqual(StatusStyle.tone(phase: "SomethingNew"), .unknown, "an unrecognised phase must not look healthy")
    }

    func testTonesUseDistinctColoursAndLabels() {
        let tones: [StatusTone] = [.running, .transitioning, .stopped, .failed, .unknown]
        XCTAssertEqual(Set(tones.map { "\($0.color)" }).count, tones.count, "two statuses share a colour")
        XCTAssertEqual(Set(tones.map(\.label)).count, tones.count, "two statuses share a label")
    }

    func testComposeProjectIsGreenOnlyWhenEveryContainerRuns() {
        XCTAssertEqual(StatusStyle.tone(compose: ["Running", "Running"]), .running)
        XCTAssertEqual(StatusStyle.tone(compose: ["Running", "Stopped"]), .transitioning)
        XCTAssertEqual(StatusStyle.tone(compose: ["Stopped", "Created"]), .stopped)
        XCTAssertEqual(StatusStyle.tone(compose: ["Running", "Dead"]), .failed)
        XCTAssertEqual(StatusStyle.tone(compose: ["Running", "Unknown"]), .unknown)
        XCTAssertEqual(StatusStyle.tone(compose: []), .unknown)
    }
}
