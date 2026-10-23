import XCTest
import SwiftUI
@testable import MacCluster

/// Renders the real Dashboard detail for different workload types with a numbered log: every page must
/// give the log the same height, never fewer than LogLayout.minimumVisibleLines, and be scrollable.
@MainActor final class LogHeightProbeTests: XCTestCase {
    private var root: URL { URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent() }

    private func pump(_ seconds: Double) {
        let end = Date().addingTimeInterval(seconds)
        while Date() < end { RunLoop.main.run(until: Date().addingTimeInterval(0.05)) }
    }

    private func workload(_ json: String) throws -> Workload { try JSONDecoder().decode(Workload.self, from: Data(json.utf8)) }

    private func allViews(_ view: NSView) -> [NSView] { [view] + view.subviews.flatMap(allViews) }

    @discardableResult
    private func snapshot(_ name: String, selecting select: (AppModel) -> Void, size: CGSize) throws -> (logHeight: CGFloat, pageScrollers: [NSScrollView]) {
        let model = AppModel()
        select(model)
        model.logs = (1...200).map { String(format: "line %03d  the quick brown fox jumps over the lazy dog", $0) }.joined(separator: "\n")
        let host = NSHostingView(rootView: Dashboard().environmentObject(model).frame(width: size.width, height: size.height))
        host.frame = NSRect(origin: .zero, size: size)
        let window = NSWindow(contentRect: host.frame, styleMask: [.titled], backing: .buffered, defer: false)
        window.contentView = host
        host.layoutSubtreeIfNeeded()
        pump(1.2)
        // The real app has no agent error here; clear what the missing agent produced in this harness.
        model.error = nil; model.inventoryError = nil; model.environmentError = nil; model.actionError = nil
        pump(0.35)
        let rep = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
        host.cacheDisplay(in: host.bounds, to: rep)
        try XCTUnwrap(rep.representation(using: .png, properties: [:])).write(to: root.appendingPathComponent(".local/shots/logprobe-\(name).png"))
        let scrollers = allViews(host).compactMap { $0 as? NSScrollView }
        let log = try XCTUnwrap(scrollers.first { $0.documentView is StripedLogTextView }, "no log view in \(name)")
        return (log.frame.height, scrollers.filter { !($0.documentView is StripedLogTextView) })
    }

    func testLogHeightIsIdenticalAcrossPagesAndPagesScroll() throws {
        let compose = try workload(#"{"uid":"c1","name":"open-notebook-surrealdb-1","namespace":"local","executor":"docker","phase":"Running","external":true,"image":"surrealdb/surrealdb:v2","ports":"0.0.0.0:8000->8000/tcp","composeProject":"open-notebook","composeService":"surrealdb","composeWorkingDir":"/path/to/open-notebook","composeConfigFiles":"/path/to/open-notebook/docker-compose.yml","containerID":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}"#)
        let plainDocker = try workload(#"{"uid":"c2","name":"ollama","namespace":"local","executor":"docker","phase":"Running","external":true,"image":"ollama/ollama","ports":"0.0.0.0:11434->11434/tcp","containerID":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}"#)
        let nativeStatus = #"{"phase":"Ready","localWorkloads":[{"uid":"n1","name":"vllm-gemma-local","namespace":"local","executor":"native","phase":"Running","pid":123,"port":8100}]}"#
        let lineHeight = LogLayout.lineHeight(fontSize: 12, lineSpacing: 0)
        for (label, size) in [("default", CGSize(width: 1218, height: 680)), ("min", CGSize(width: 960, height: 628)), ("tall", CGSize(width: 1218, height: 1100))] {
            let c = try snapshot("compose-\(label)", selecting: { $0.externalContainers = [compose]; $0.selected = .workload("", "c1") }, size: size)
            let d = try snapshot("docker-\(label)", selecting: { $0.externalContainers = [plainDocker]; $0.selected = .workload("", "c2") }, size: size)
            let n = try snapshot("native-\(label)", selecting: { $0.status = try? JSONDecoder().decode(AgentStatus.self, from: Data(nativeStatus.utf8)); $0.selected = .workload("", "n1") }, size: size)
            let lines = [c, d, n].map { Int(($0.logHeight - 28) / lineHeight) }
            print("LOGHEIGHT \(label): compose=\(c.logHeight) docker=\(d.logHeight) native=\(n.logHeight) -> visible lines \(lines)")
            XCTAssertEqual(c.logHeight, d.logHeight, accuracy: 0.5, "\(label): compose and docker differ")
            XCTAssertEqual(c.logHeight, n.logHeight, accuracy: 0.5, "\(label): compose and native differ")
            XCTAssertGreaterThanOrEqual(c.logHeight, LogLayout.minimumHeight(fontSize: 12, lineSpacing: 0) - 0.5, "\(label): below the minimum")
            XCTAssertFalse(c.pageScrollers.isEmpty, "\(label): the page itself is not scrollable")
        }
    }

    func testRenderStatusColours() throws {
        func c(_ n: Int, _ name: String, _ phase: String) -> String {
            #"{"uid":"x\#(n)","name":"\#(name)","namespace":"local","executor":"docker","phase":"\#(phase)","external":true,"image":"img","composeProject":"demo","composeService":"\#(name)","containerID":"\#(String(repeating: "a", count: 64))"}"#
        }
        let model = AppModel()
        let phases = ["Running", "Restarting", "Stopped", "Dead", "Unknown"]
        model.externalContainers = try phases.enumerated().map { try workload(c($0.offset, "svc-\($0.element.lowercased())", $0.element)) }
        model.selected = .compose("demo")
        let host = NSHostingView(rootView: Dashboard().environmentObject(model).frame(width: 1100, height: 760))
        host.frame = NSRect(x: 0, y: 0, width: 1100, height: 760)
        let window = NSWindow(contentRect: host.frame, styleMask: [.titled], backing: .buffered, defer: false)
        window.contentView = host
        host.layoutSubtreeIfNeeded()
        pump(1.2)
        model.error = nil; model.inventoryError = nil; model.environmentError = nil; model.actionError = nil
        pump(0.35)
        let rep = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
        host.cacheDisplay(in: host.bounds, to: rep)
        try XCTUnwrap(rep.representation(using: .png, properties: [:])).write(to: root.appendingPathComponent(".local/shots/status-colours-compose.png"))
    }
}
