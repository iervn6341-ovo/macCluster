import Foundation
import XCTest
@testable import MacCluster

@MainActor final class AgentRecoveryTests: XCTestCase {
    private actor Server {
        var dockerReady = false
        var agentReady = false
        var holdStatus = false
        var pendingStatus: CheckedContinuation<Void, Never>?
        var starts = 0
        var stops = 0
        var failsStop = false
        func failStop() { failsStop = true }
        func stop() throws { stops += 1; if failsStop { throw ClientError.timedOut } }
        var failsStart = true
        func allowStart() { failsStart = false }
        func ready(docker: Bool, agent: Bool) { dockerReady = docker; agentReady = agent }
        func hold() { holdStatus = true }
        func release() { holdStatus = false; pendingStatus?.resume(); pendingStatus = nil }
        func start() throws { starts += 1; if failsStart { throw ClientError.timedOut } }
        func command(_ args: [String]) async throws -> Data {
            switch args.first {
            case "status":
                if holdStatus { await withCheckedContinuation { pendingStatus = $0 } }
                guard agentReady else { throw ClientError.message("agent unavailable") }
                return Data(#"{"phase":"Ready","clusters":[],"localWorkloads":[]}"#.utf8)
            case "inventory":
                guard dockerReady else { throw ClientError.message("cannot connect to docker.sock") }
                return Data(#"[{"uid":"docker-external-test","name":"test","namespace":"local","executor":"docker","phase":"Running","external":true}]"#.utf8)
            default: return Data("[]".utf8)
            }
        }
    }
    private func model(_ server: Server) -> AppModel {
        AppModel(refreshCommand: { try await server.command($0) }, startAgent: { try await server.start() }, stopAgent: { try await server.stop() }, startupStatus: { try await server.command(["status"]) }, startupTimeout: .milliseconds(80), startupPollInterval: .milliseconds(5), maintainsTerminalLogs: false)
    }

    func testDisconnectedStatusNeverRunsLocalTool() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let tool = directory.appendingPathComponent("tool")
        let marker = directory.appendingPathComponent("called")
        try Data("#!/bin/sh\ntouch '\(marker.path)'\nprintf '{}'\n".utf8).write(to: tool)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: tool.path)
        do {
            _ = try await AgentClient.command(["status"], timeout: 0.1, stateDirectory: directory, toolsExecutable: tool)
            XCTFail("a missing socket cannot report success")
        } catch ClientError.agentUnavailable { }
        XCTAssertFalse(FileManager.default.fileExists(atPath: marker.path), "status must not spawn a local tool or fall back to a CLI")
    }

    func testSocketLogRequestPreservesQueryValues() throws {
        let request = try XCTUnwrap(AgentClient.socketRequest(["logs", "--uid", "pod & value", "--cluster-id", "cluster/?x"]))
        XCTAssertEqual(request.method, "GET")
        let url = try XCTUnwrap(URLComponents(string: "http://localhost" + request.path))
        XCTAssertEqual(url.queryItems, [URLQueryItem(name: "uid", value: "pod & value"), URLQueryItem(name: "clusterID", value: "cluster/?x")])
        XCTAssertEqual(AgentClient.socketRequest(["drain"])?.method, "POST")
        XCTAssertNil(AgentClient.socketRequest(["inventory"]))
    }

    func testServiceCommandsDoNotReceiveUnsupportedStateDirectory() {
        let state = URL(fileURLWithPath: "/tmp/test state")
        for action in ["start", "stop", "restart", "status"] {
            XCTAssertEqual(AgentClient.commandArguments(["service", action], stateDirectory: state), ["service", action])
        }
        for action in ["config", "install"] {
            XCTAssertEqual(AgentClient.commandArguments(["service", action], stateDirectory: state), ["service", action, "--state-dir", state.path])
        }
        XCTAssertEqual(AgentClient.commandArguments(["status"], stateDirectory: state), ["status", "--state-dir", state.path])
    }

    func testDelayedReadinessKeepsProgressVisibleUntilConfirmed() async throws {
        let server = Server()
        await server.allowStart()
        await server.ready(docker: true, agent: true)
        await server.hold()
        let app = model(server)
        let task = Task { await app.startService() }
        for _ in 0..<100 {
            if app.serviceStarting { break }
            try await Task.sleep(for: .milliseconds(1))
        }
        XCTAssertTrue(app.serviceStarting)
        XCTAssertTrue(app.busy)
        XCTAssertEqual(app.startupNotice, .starting)
        await server.release()
        await task.value
        XCTAssertFalse(app.serviceStarting)
        XCTAssertFalse(app.busy)
        XCTAssertEqual(app.startupNotice, .ready)
    }

    func testSingleBriefResponseDoesNotCountAsStableStartup() async {
        actor Replies {
            var count = 0
            func probe() throws -> Data {
                count += 1
                if count == 1 { return Data(#"{"phase":"Ready","clusters":[],"localWorkloads":[]}"#.utf8) }
                throw ClientError.message("agent exited")
            }
        }
        let replies = Replies()
        let app = AppModel(refreshCommand: { _ in Data("[]".utf8) }, startAgent: {},
                           startupStatus: { try await replies.probe() }, startupTimeout: .milliseconds(40),
                           startupPollInterval: .milliseconds(5), maintainsTerminalLogs: false)
        await app.startService()
        XCTAssertEqual(app.startupNotice, .waiting)
        XCTAssertNotNil(app.startupError)
        XCTAssertFalse(app.serviceStarting)
        XCTAssertFalse(app.busy)
    }

    func testRestartWithAlreadyStoppedServiceStillStartsAgent() async {
        let server = Server()
        await server.allowStart()
        await server.ready(docker: true, agent: true)
        let app = model(server)
        await app.restartService()
        let starts = await server.starts, stops = await server.stops
        XCTAssertEqual(stops, 1)
        XCTAssertEqual(starts, 1)
        XCTAssertEqual(app.startupNotice, .ready)
        XCTAssertNil(app.actionError)
        XCTAssertFalse(app.busy)
    }

    func testStopFailureDoesNotClaimStoppedOrAttemptRestart() async {
        let server = Server()
        await server.failStop()
        let app = model(server)
        await app.restartService()
        let starts = await server.starts
        XCTAssertEqual(starts, 0)
        XCTAssertNotNil(app.startupError)
        XCTAssertFalse(app.busy)
        await app.stopService()
        XCTAssertNil(app.notice)
        XCTAssertNotNil(app.actionError)
    }

    func testStartupFailureStopsBusyAndDockerRecoversWithoutRestartingApp() async {
        let server = Server()
        // Use the same transport for every stage of this app session.
        let app = self.model(server)
        await app.initialize()
        XCTAssertFalse(app.busy)
        XCTAssertNotNil(app.startupError)
        XCTAssertNotNil(app.inventoryError)
        await server.ready(docker: true, agent: false)
        let agentConnected = await app.refresh()
        XCTAssertFalse(agentConnected)
        XCTAssertNil(app.inventoryError, "Docker must recover independently of the agent")
        XCTAssertEqual(app.externalContainers.map(\.uid), ["docker-external-test"])
        XCTAssertFalse(app.busy)
        await server.ready(docker: true, agent: true)
        let connected = await app.refresh()
        XCTAssertTrue(connected)
        XCTAssertNil(app.startupError)
        XCTAssertNil(app.error)
        let starts = await server.starts
        XCTAssertEqual(starts, 1)
    }

    func testWaitingNoticeBecomesReadyOnLaterRefreshAndPreservesOperationNotice() async {
        let server = Server()
        await server.allowStart()
        await server.ready(docker: true, agent: false)
        let model = self.model(server)
        await model.startService()
        XCTAssertEqual(model.startupNotice, .waiting)
        XCTAssertFalse(model.busy)
        await server.ready(docker: true, agent: true)
        await model.refresh()
        XCTAssertEqual(model.startupNotice, .ready)
        XCTAssertNil(model.notice)

        // A later status refresh must not replace another operation's result.
        model.notice = "已建立服務，啟動狀態將顯示於清單。"
        await model.refresh()
        XCTAssertEqual(model.notice, "已建立服務，啟動狀態將顯示於清單。")
        XCTAssertEqual(model.startupNotice, .ready)
    }

    func testSlowStatusDoesNotDelayDockerInventoryAndOverlappingRefreshesShareResult() async {
        let server = Server()
        await server.ready(docker: true, agent: true)
        await server.hold()
        let model = self.model(server)
        let first = Task { await model.refresh() }
        let deadline = Date().addingTimeInterval(2)
        while model.externalContainers.isEmpty && Date() < deadline {
            try? await Task.sleep(for: .milliseconds(10))
        }
        XCTAssertEqual(model.externalContainers.count, 1, "inventory should appear while status is still waiting")
        let initialize = Task { await model.initialize() }
        try? await Task.sleep(for: .milliseconds(30))
        await server.release()
        let connected = await first.value
        await initialize.value
        XCTAssertTrue(connected)
        XCTAssertFalse(model.busy)
        let starts = await server.starts
        XCTAssertEqual(starts, 0, "an in-flight refresh must not be mistaken for a disconnected agent")
    }

    func testCommandTimeoutKillsAProcessThatIgnoresTermination() async throws {
        let start = Date()
        do {
            _ = try await AgentClient.run(URL(fileURLWithPath: "/bin/sh"),
                ["-c", "trap '' TERM; exec /bin/sleep 30"], timeout: 0.1)
            XCTFail("a command that ignores SIGTERM must time out")
        } catch ClientError.timedOut { }
        XCTAssertLessThan(Date().timeIntervalSince(start), 2)
        let output = try await AgentClient.run(URL(fileURLWithPath: "/bin/sh"), ["-c", "printf recovered"], timeout: 1)
        XCTAssertEqual(String(decoding: output, as: UTF8.self), "recovered")
    }

    func testLoadedAgentWithOldTranslocatedPathRequiresReload() throws {
        let arguments = ["/Applications/MacCluster.app/Contents/Resources/maccluster-agent", "serve", "--state-dir", "/tmp/test state"]
        let old = ["/private/tmp/AppTranslocation/old/MacCluster.app/Contents/Resources/maccluster-agent"] + arguments.dropFirst()
        let output = "job = {\n\targuments = {\n" + old.map { "\t\t" + $0 }.joined(separator: "\n") + "\n\t}\n}"
        XCTAssertFalse(try LaunchAgentConfiguration.matches(Data(output.utf8), arguments: arguments))
        let current = "job = {\n\targuments = {\n" + arguments.map { "\t\t" + $0 }.joined(separator: "\n") + "\n\t}\n}"
        XCTAssertTrue(try LaunchAgentConfiguration.matches(Data(current.utf8), arguments: arguments))
        var differentState = arguments
        differentState[3] = "/tmp/other state"
        XCTAssertFalse(try LaunchAgentConfiguration.matches(Data(current.utf8), arguments: differentState))
    }

    func testUnrecognizedLaunchdOutputDoesNotTriggerBlindReload() {
        XCTAssertThrowsError(try LaunchAgentConfiguration.matches(Data("unknown format".utf8), arguments: []))
    }

}
