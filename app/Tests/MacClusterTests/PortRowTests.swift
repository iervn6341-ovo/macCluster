import XCTest
@testable import MacCluster

final class PortRowTests: XCTestCase {
    private func workload(_ fields: String, executor: String = "docker") throws -> Workload {
        let json = #"{"uid":"u","name":"n","namespace":"ns","executor":"\#(executor)","phase":"Running",\#(fields)}"#
        return try JSONDecoder().decode(Workload.self, from: Data(json.utf8))
    }

    func testIPv4AndIPv6OfTheSameMappingBecomeOneRow() {
        let (rows, unpublished) = PortRow.parse("0.0.0.0:11432->11434/tcp, [::]:11432->11434/tcp")
        XCTAssertEqual(rows, [PortRow(hostPort: "11432", reach: PortRow.allInterfaces, target: "11434/tcp")])
        XCTAssertTrue(unpublished.isEmpty)
    }

    func testLoopbackSpecificAddressRangesAndDeclaredOnlyPorts() {
        let (rows, unpublished) = PortRow.parse("127.0.0.1:51435->80/tcp, 192.0.2.5:9000->9000/udp, 0.0.0.0:8000-8010->8000-8010/tcp, 5432/tcp")
        XCTAssertEqual(rows.map(\.hostPort), ["51435", "9000", "8000-8010"])
        XCTAssertEqual(rows.map(\.reach), [PortRow.loopback, "192.0.2.5", PortRow.allInterfaces])
        XCTAssertEqual(unpublished, ["5432/tcp"])
    }

    func testClusterPodAndLocalContainerUseTheSameRows() throws {
        let local = try workload(#""external":true,"ports":"127.0.0.1:51435->80/tcp","portMappings":"127.0.0.1:51435->80/tcp""#)
        let pod = try workload(#""port":51435,"portMappings":"127.0.0.1:51435->80/tcp""#)
        XCTAssertEqual(PortRow.rows(for: local).rows, PortRow.rows(for: pod).rows)
        XCTAssertEqual(PortRow.rows(for: pod).rows.first?.target, "80/tcp")
    }

    func testOlderAgentWithoutMappingsStillShowsAPort() throws {
        let pod = try workload(#""port":51435"#)
        XCTAssertEqual(PortRow.rows(for: pod).rows, [PortRow(hostPort: "51435", reach: PortRow.loopback, target: nil)])
    }

    func testNativeFixedAndDynamicPorts() throws {
        let fixed = try workload(#""fixedPorts":[8100,9100],"port":8100"#, executor: "native")
        XCTAssertEqual(PortRow.rows(for: fixed).rows.map(\.hostPort), ["8100", "9100"])
        let dynamic = try workload(#""port":53001"#, executor: "native")
        XCTAssertEqual(PortRow.rows(for: dynamic).rows.map(\.hostPort), ["53001"])
        let none = try workload(#""port":0"#, executor: "native")
        XCTAssertTrue(PortRow.rows(for: none).rows.isEmpty)
    }

    func testNoPortsMeansNoRows() throws {
        let local = try workload(#""external":true,"ports":"""#)
        XCTAssertTrue(PortRow.rows(for: local).rows.isEmpty)
    }
}
