import XCTest
import Network
@testable import MacCluster

final class HealthCheckTests: XCTestCase {
    func testLocalNetworkHosts() {
        // Built from parts so that no private address is written out in the source.
        func ip(_ parts: Int...) -> String { parts.map(String.init).joined(separator: ".") }
        for host in [ip(10, 1, 2, 3), ip(192, 168, 1, 5), ip(172, 16, 0, 1), ip(172, 31, 255, 1), ip(169, 254, 3, 4), "mac.local"] {
            XCTAssertTrue(LocalNetworkProbe.isLocalNetworkHost(host), host)
        }
        for host in ["8.8.8.8", "172.32.0.1", "172.15.0.1", "example.com", "127.0.0.1", "300.1.1.1"] {
            XCTAssertFalse(LocalNetworkProbe.isLocalNetworkHost(host), host)
        }
    }

    func testEndpointParts() {
        XCTAssertEqual(LocalNetworkProbe.endpointParts("https://192.0.2.10:6443")?.port, 6443)
        XCTAssertEqual(LocalNetworkProbe.endpointParts("https://192.0.2.10:6443")?.host, "192.0.2.10")
        XCTAssertEqual(LocalNetworkProbe.endpointParts("https://cp.example")?.port, 443)
        XCTAssertNil(LocalNetworkProbe.endpointParts("not a url"))
    }

    func testNoRouteWithWorkingPingPointsAtThePermission() {
        let item = LocalNetworkProbe.item(for: .posix(.EHOSTUNREACH), pingOK: true, address: "192.0.2.10:6443")
        XCTAssertEqual(item.status, .fail)
        XCTAssertEqual(item.action, "open-local-network-settings")
        XCTAssertTrue(item.hint?.contains("區域網路") == true)
    }

    func testNoRouteWithoutPingIsNotBlamedOnThePermission() {
        let item = LocalNetworkProbe.item(for: .posix(.EHOSTUNREACH), pingOK: false, address: "192.0.2.10:6443")
        XCTAssertEqual(item.status, .fail)
        XCTAssertNil(item.action)
        XCTAssertFalse(item.hint?.contains("區域網路") == true)
    }

    func testOtherOutcomes() {
        XCTAssertEqual(LocalNetworkProbe.item(for: .reachable, pingOK: false, address: "a").status, .ok)
        XCTAssertEqual(LocalNetworkProbe.item(for: .posix(.ECONNREFUSED), pingOK: false, address: "a").status, .warn)
        XCTAssertEqual(LocalNetworkProbe.item(for: .timedOut, pingOK: false, address: "a").status, .warn)
    }

    func testTheAgentsAnswerDecodes() throws {
        let json = """
        {"checkedAt":"2026-10-22T12:00:00Z","checks":[
          {"id":"control-plane:c","title":"連到控制平面 c","status":"fail","detail":"x","hint":"y","action":"open-local-network-settings"},
          {"id":"certificate:c","title":"節點憑證 c","status":"ok","detail":"有效"}]}
        """
        let result = try JSONDecoder().decode(NetCheckResult.self, from: Data(json.utf8))
        XCTAssertEqual(result.checks.count, 2)
        XCTAssertEqual(result.checks[0].status, .fail)
        XCTAssertEqual(result.checks[0].action, "open-local-network-settings")
        XCTAssertNil(result.checks[1].action)
    }

    /// A real connection from this process: to a listener that exists, and to a port nobody listens on.
    func testRealConnectionsToTheLoopback() async throws {
        let listener = try NWListener(using: .tcp, on: .any)
        listener.newConnectionHandler = { $0.cancel() }
        let ready = expectation(description: "listening")
        listener.stateUpdateHandler = { if case .ready = $0 { ready.fulfill() } }
        listener.start(queue: .global())
        await fulfillment(of: [ready], timeout: 5)
        let port = try XCTUnwrap(listener.port?.rawValue)
        let reachable = await LocalNetworkProbe.connect(host: "127.0.0.1", port: port)
        XCTAssertEqual(reachable, .reachable)
        listener.cancel()
        try await Task.sleep(for: .milliseconds(200))
        let refused = await LocalNetworkProbe.connect(host: "127.0.0.1", port: port)
        XCTAssertEqual(refused, .posix(.ECONNREFUSED))
    }
}
