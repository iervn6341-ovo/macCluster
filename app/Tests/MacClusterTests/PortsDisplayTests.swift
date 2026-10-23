import XCTest
@testable import MacCluster

final class PortsDisplayTests: XCTestCase {
    func testConfiguredPortsFlagDecodesAlongsideTheLivePortsString() throws {
        let json = #"""
        {"phase":"Ready","localWorkloads":[
          {"uid":"a","name":"open-webui","namespace":"local","executor":"docker","phase":"Stopped","external":true,"ports":"0.0.0.0:3000->8080/tcp","portsConfigured":true,"pid":0,"port":0},
          {"uid":"b","name":"ollama","namespace":"local","executor":"docker","phase":"Running","external":true,"ports":"0.0.0.0:11432->11434/tcp"}
        ]}
        """#
        let status = try JSONDecoder().decode(AgentStatus.self, from: Data(json.utf8))
        let w = try XCTUnwrap(status.localWorkloads)
        XCTAssertEqual(w[0].portsConfigured, true)
        XCTAssertEqual(w[0].ports, "0.0.0.0:3000->8080/tcp")
        XCTAssertNil(w[1].portsConfigured)
    }
}
