import XCTest
import SwiftUI
import SwiftTerm
@testable import MacCluster

/// Drives the real embedded terminal against the real agent CLI and registry in
/// the repo's .local state directory, then renders the detail view offscreen.
@MainActor final class EnvironmentViewTests: XCTestCase {
    private var root: URL { URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent() }

    private func agent(_ args: [String]) throws -> Data {
        let binary = root.appendingPathComponent("dist/maccluster-agent")
        try XCTSkipUnless(FileManager.default.isExecutableFile(atPath: binary.path), "dist/maccluster-agent not built")
        let state = root.appendingPathComponent(".local/agent")
        try XCTSkipUnless(FileManager.default.fileExists(atPath: state.appendingPathComponent("environments/vllm-metal/env.json").path), "vllm-metal environment not registered")
        let p = Process()
        p.executableURL = binary
        p.arguments = args + ["--state-dir", state.path]
        let out = Pipe()
        p.standardOutput = out
        try p.run()
        let data = out.fileHandleForReading.readDataToEndOfFile()
        p.waitUntilExit()
        XCTAssertEqual(p.terminationStatus, 0)
        return data
    }

    private func pump(_ seconds: Double) {
        let end = Date().addingTimeInterval(seconds)
        while Date() < end { RunLoop.main.run(until: Date().addingTimeInterval(0.05)) }
    }

    private func dump(_ view: LocalProcessTerminalView) -> String {
        String(data: view.getTerminal().getBufferAsData(), encoding: .utf8) ?? ""
    }

    func testEmbeddedTerminalRunsInsideEnvironmentAndRendersDetail() throws {
        let list = try JSONDecoder().decode([EnvStatus].self, from: try agent(["env", "list", "--json"]))
        let env = try XCTUnwrap(list.first { $0.name == "vllm-metal" })
        XCTAssertTrue(env.available)
        XCTAssertTrue(env.labelSnippet.contains("env.maccluster.example.com/vllm-metal"))

        let spec = try JSONDecoder().decode(ShellSpec.self, from: try agent(["env", "shell", "--name", "vllm-metal"]))
        let sessions = TerminalSessions()
        sessions.open("vllm-metal", spec: spec)
        XCTAssertTrue(sessions.running.contains("vllm-metal"))
        let view = try XCTUnwrap(sessions.view(for: "vllm-metal"))

        // Lay the view out at its final size before the user types, as the app does.
        let model = AppModel()
        model.environments = list
        let detail = EnvironmentDetailView(terminals: sessions, environment: env, edit: {}, remove: {})
            .environmentObject(model)
            .padding(22)
            .frame(width: 900, height: 720)
            .appScreen()
        let host = NSHostingView(rootView: detail)
        host.frame = NSRect(x: 0, y: 0, width: 900, height: 720)
        let window = NSWindow(contentRect: host.frame, styleMask: [.titled], backing: .buffered, defer: false)
        window.contentView = host
        host.layoutSubtreeIfNeeded()
        pump(2.5)

        view.send(txt: "echo MARK_BEGIN; which vllm; echo venv=$VIRTUAL_ENV plugins=$VLLM_PLUGINS; pwd; echo MARK_END\n")
        pump(2.5)
        let text = dump(view)
        try? text.write(to: root.appendingPathComponent(".local/shots/terminal-buffer.txt"), atomically: true, encoding: .utf8)
        XCTAssertTrue(text.contains(env.root + "/bin/vllm"), text)
        XCTAssertTrue(text.contains("venv=" + env.root), text)
        XCTAssertTrue(text.contains("plugins=metal"), text)
        XCTAssertTrue(text.contains("MARK_END"), text)

        let rep = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
        host.cacheDisplay(in: host.bounds, to: rep)
        try XCTUnwrap(rep.representation(using: .png, properties: [:])).write(to: root.appendingPathComponent(".local/shots/env-detail-with-terminal.png"))

        // Measure, don't assume: does a later resize keep earlier output under interactive zsh?
        host.frame = NSRect(x: 0, y: 0, width: 760, height: 720)
        host.layoutSubtreeIfNeeded()
        pump(1.5)
        let resized = dump(view)
        try? resized.write(to: root.appendingPathComponent(".local/shots/terminal-after-later-resize.txt"), atomically: true, encoding: .utf8)
        try? "survivesLaterResize=\(resized.contains("MARK_END"))".write(to: root.appendingPathComponent(".local/shots/terminal-size.txt"), atomically: true, encoding: .utf8)

        sessions.close("vllm-metal")
        XCTAssertFalse(sessions.running.contains("vllm-metal"))
    }

    func testResizeKeepsOutputForPlainShellAndZsh() throws {
        for (label, exe, args) in [("sh", "/bin/sh", ["-c", "echo KEEP_ME; sleep 6"]), ("zsh-f", "/bin/zsh", ["-f", "-i", "-c", "echo KEEP_ME; sleep 6"])] {
            let view = LocalProcessTerminalView(frame: NSRect(x: 0, y: 0, width: 640, height: 320))
            view.startProcess(executable: exe, args: args, environment: ["TERM=xterm-256color", "PATH=/usr/bin:/bin"], execName: nil, currentDirectory: "/tmp")
            pump(1.5)
            view.frame = NSRect(x: 0, y: 0, width: 856, height: 320)
            view.layoutSubtreeIfNeeded()
            pump(1.0)
            let whole = String(data: view.getTerminal().getBufferAsData(), encoding: .utf8) ?? ""
            try? whole.write(to: root.appendingPathComponent(".local/shots/resize-\(label).txt"), atomically: true, encoding: .utf8)
            XCTAssertTrue(whole.contains("KEEP_ME"), "\(label): output lost after resize: \(whole)")
            view.terminate()
        }
    }

    private func render<V: View>(_ view: V, width: CGFloat, height: CGFloat, to name: String, wait: Double = 1.0) throws {
        let host = NSHostingView(rootView: view.frame(width: width, height: height))
        host.frame = NSRect(x: 0, y: 0, width: width, height: height)
        let window = NSWindow(contentRect: host.frame, styleMask: [.titled], backing: .buffered, defer: false)
        window.contentView = host
        host.layoutSubtreeIfNeeded()
        pump(wait)
        let rep = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
        host.cacheDisplay(in: host.bounds, to: rep)
        try XCTUnwrap(rep.representation(using: .png, properties: [:])).write(to: root.appendingPathComponent(".local/shots/\(name).png"))
    }

    func testRenderFormAndDashboard() throws {
        let list = try JSONDecoder().decode([EnvStatus].self, from: try agent(["env", "list", "--json"]))
        let model = AppModel()
        model.environments = list
        try render(EnvironmentFormView(editing: nil).environmentObject(model), width: 680, height: 640, to: "env-form-new")
        try render(EnvironmentFormView(editing: list[0]).environmentObject(model), width: 680, height: 640, to: "env-form-edit")
        model.selected = .environment("vllm-metal")
        try render(Dashboard().environmentObject(model), width: 1200, height: 760, to: "dashboard-env", wait: 2.0)
    }

    func testRenderHelpPages() throws {
        let model = AppModel()
        func page<V: View>(_ v: V, _ name: String, height: CGFloat) throws {
            try render(ScrollView { v.padding(16) }.environmentObject(model).appScreen(), width: 740, height: height, to: "help-" + name, wait: 0.8)
        }
        try page(GeneralHelpPage(), "general", height: 720)
        try page(ContainersHelpPage(), "containers", height: 1300)
        try page(NativeHelpPage(), "native", height: 1500)
        try page(ClusterHelpPage(), "cluster", height: 640)
        try page(NetworkHelpPage(), "network", height: 640)
        try page(YAMLHelpPage(), "yaml", height: 1700)
    }
}
