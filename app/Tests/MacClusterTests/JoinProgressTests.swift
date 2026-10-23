import XCTest
@testable import MacCluster

@MainActor final class JoinProgressTests: XCTestCase {
    private actor Backend {
        var phase = "Joining"
        var message = "Verifying CA pin"
        func set(_ phase: String, _ message: String) { self.phase = phase; self.message = message }
        func refresh(_ args: [String]) throws -> Data {
            guard args.first == "status" else { return Data("[]".utf8) }
            return try JSONSerialization.data(withJSONObject: [
                "phase": "Ready", "clusters": [["id": "new", "name": "test", "endpoint": "https://192.0.2.10:6443",
                "nodeName": "maccluster-test", "phase": phase, "message": message, "managementConfigured": false]],
                "localWorkloads": []
            ])
        }
    }

    private func model(_ backend: Backend) -> AppModel {
        AppModel(refreshCommand: { try await backend.refresh($0) }, operationCommand: { verb, _, sensitive in
            XCTAssertEqual(verb, "join"); XCTAssertTrue(sensitive)
            return Data(#"{"accepted":true,"id":"new"}"#.utf8)
        }, maintainsTerminalLogs: false)
    }

    func testAcceptedJoinAdvancesToReady() async throws {
        let backend = Backend()
        let realApp = model(backend)
        try await realApp.perform("join", ["command": "mock"], sensitive: true)
        XCTAssertTrue(realApp.joinNotice?.contains("Verifying CA pin") == true)
        XCTAssertFalse(realApp.busy)
        await backend.set("Ready", "Connected")
        await realApp.refresh()
        XCTAssertEqual(realApp.joinNotice, "已加入叢集：test。")
        XCTAssertNil(realApp.joinError)
    }

    func testAcceptedJoinFailureReplacesWaitingAndPreservesOtherNotice() async throws {
        let backend = Backend(), app = model(backend)
        try await app.perform("join", ["command": "mock"], sensitive: true)
        app.notice = "其他操作結果"
        await backend.set("Error", "node name must be DNS-compatible and start with maccluster-")
        await app.refresh()
        XCTAssertNil(app.joinNotice)
        XCTAssertTrue(app.joinError?.contains("Kubernetes") == true)
        XCTAssertEqual(app.notice, "其他操作結果")
        XCTAssertFalse(app.busy)
    }

    func testImmediateAsyncFailureDoesNotLeaveWaitingNotice() async throws {
        let backend = Backend(), app = model(backend)
        await backend.set("Error", "bootstrap timed out")
        try await app.perform("join", ["command": "mock"], sensitive: true)
        XCTAssertNil(app.joinNotice)
        XCTAssertEqual(app.joinError, "加入叢集失敗：bootstrap timed out")
        await backend.set("Ready", "Recovered")
        await app.refresh()
        XCTAssertNil(app.joinError)
        XCTAssertEqual(app.joinNotice, "已加入叢集：test。")
    }

    func testDefaultNodeNameIsVisibleValidAndDistinct() throws {
        let id = UUID(uuidString: "01234567-89AB-CDEF-0123-456789ABCDEF")!
        XCTAssertEqual(JoinValidation.defaultNodeName(hostName: "My MacBook.local", identifier: id), "my-macbook-01234567")
        for host in ["", "我的電腦", "---", "maccluster-", "MACCLUSTER-Test.local", String(repeating: "a", count: 300)] {
            let generated = JoinValidation.defaultNodeName(hostName: host, identifier: id)
            XCTAssertFalse(generated.isEmpty)
            XCTAssertLessThanOrEqual(generated.utf8.count, 63)
            XCTAssertNoThrow(try JoinValidation.validateNodeName(generated))
        }
        XCTAssertNotEqual(JoinValidation.defaultNodeName(hostName: "mac"), JoinValidation.defaultNodeName(hostName: "mac"))
    }

    func testNodeNamesAllowAutomaticDefaultAndCustomHostname() {
        for name in ["", "my-macbook-pro", "worker01", "maccluster-macbook-pro", "maccluster-node.local"] {
            XCTAssertNoThrow(try JoinValidation.validateNodeName(name))
        }
        for name in ["my_macbook_pro", "maccluster-UPPER", "maccluster-a_", "maccluster-a.", "maccluster-a\n", "maccluster-a ", "maccluster-", "maccluster-" + String(repeating: "a", count: 250)] {
            XCTAssertThrowsError(try JoinValidation.validateNodeName(name))
        }
    }

    func testHealthPageUsesLabelsWithoutChangingDiagnosticData() {
        let endpoint = "https://192.0.2.10:6443"
        let raw = HealthCheckItem(id: "x", title: "連到控制平面 \(endpoint)", status: .fail,
                                  detail: "agent 連 192.0.2.10:6443 失敗", hint: "ping 192.0.2.10", action: "open-local-network-settings")
        let displayed = raw.displayed(endpoints: [endpoint])
        XCTAssertFalse([displayed.title, displayed.detail ?? "", displayed.hint ?? ""].joined().contains("192.0.2.10"))
        XCTAssertTrue(raw.detail?.contains("192.0.2.10") == true)
        XCTAssertEqual(displayed.action, raw.action)
        XCTAssertEqual(displayed.status, .fail)
    }
}
