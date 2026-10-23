import XCTest
@testable import MacCluster

final class StatusDecodingTests: XCTestCase {
    /// `ports` is a string for external Docker containers; fixed ports of native
    /// workloads must use a different key or one of the two fails to decode.
    func testFixedPortsAndDockerPortsStringDecodeTogether() throws {
        let json = #"""
        {"phase":"Ready","localWorkloads":[
          {"uid":"a","name":"vllm","namespace":"local","executor":"native","phase":"Running","port":8100,"fixedPorts":[8100,9100]},
          {"uid":"b","name":"nginx","namespace":"local","executor":"docker","phase":"Running","external":true,"ports":"0.0.0.0:8080->80/tcp"}
        ]}
        """#
        let status = try JSONDecoder().decode(AgentStatus.self, from: Data(json.utf8))
        let workloads = try XCTUnwrap(status.localWorkloads)
        XCTAssertEqual(workloads[0].fixedPorts, [8100, 9100])
        XCTAssertNil(workloads[0].ports)
        XCTAssertEqual(workloads[1].ports, "0.0.0.0:8080->80/tcp")
        XCTAssertNil(workloads[1].fixedPorts)
    }
}

final class KeepAwakeStatusDecodingTests: XCTestCase {
    func testStatusCarriesTheKeepAwakeState() throws {
        let json = #"{"phase":"Ready","keepAwake":{"mode":"always","held":true}}"#
        let status = try JSONDecoder().decode(AgentStatus.self, from: Data(json.utf8))
        XCTAssertEqual(status.keepAwake, KeepAwakeStatus(mode: "always", held: true))
    }
    func testAnOlderAgentWithoutTheFieldStillDecodes() throws {
        let status = try JSONDecoder().decode(AgentStatus.self, from: Data(#"{"phase":"Ready"}"#.utf8))
        XCTAssertNil(status.keepAwake)
    }
}

final class DockerLoginDecodingTests: XCTestCase {
    func testLoginListDecodesNamesOnly() throws {
        let json = #"[{"registry":"https://index.docker.io/v1/","store":"desktop"},{"registry":"registry.example.com"}]"#
        let list = try JSONDecoder().decode([DockerLogin].self, from: Data(json.utf8))
        XCTAssertEqual(list.map(\.display), ["Docker Hub", "registry.example.com"])
        XCTAssertEqual(list[0].store, "desktop")
        XCTAssertNil(list[1].store)
    }
    func testLoginRequestCarriesThePasswordInTheJSONBodyOnly() throws {
        let data = try JSONEncoder().encode(DockerLoginRequest(registry: "r.example.com", username: "alice", password: "p w"))
        let object = try JSONSerialization.jsonObject(with: data) as? [String: String]
        XCTAssertEqual(object, ["registry": "r.example.com", "username": "alice", "password": "p w"])
    }
}
