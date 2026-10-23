import AppKit
import SwiftUI
import QuartzCore

/// Opt-in, deterministic profiling. Never contacts the agent or starts a workload.
@MainActor enum SidebarDiagnostics {
    static let directory = ProcessInfo.processInfo.environment["MACCLUSTER_SIDEBAR_PROFILE"]
    static var enabled: Bool { directory != nil }
    static var expanded = true
    static let surface = ProcessInfo.processInfo.environment["MACCLUSTER_SIDEBAR_SURFACE"] ?? "glass"
    static let detail = ProcessInfo.processInfo.environment["MACCLUSTER_SIDEBAR_DETAIL"] ?? "normal"
    static let hideToolbar = enabled && ProcessInfo.processInfo.environment["MACCLUSTER_SIDEBAR_TOOLBAR"] == "hidden"

    static func configure(_ model: AppModel) {
        guard enabled else { return }
        let fixture = #"{"phase":"Ready","localWorkloads":[{"uid":"demo","name":"native-http-demo","namespace":"local","executor":"native","phase":"Running","pid":123,"port":8100}]}"#
        model.status = try? JSONDecoder().decode(AgentStatus.self, from: Data(fixture.utf8))
        if ProcessInfo.processInfo.environment["MACCLUSTER_SIDEBAR_PAGE"] == "logs" {
            model.selected = .workload("", "demo")
            model.logs = (1...10000).map { "line \($0): " + String(repeating: "long diagnostic log text ", count: 8) }.joined(separator: "\n")
        }
    }
}

struct SidebarDiagnosticDetail: ViewModifier {
    @ViewBuilder func body(content: Content) -> some View {
        if SidebarDiagnostics.enabled && SidebarDiagnostics.detail == "empty" {
            Color.clear.frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if SidebarDiagnostics.enabled && SidebarDiagnostics.detail == "fixed" {
            content.frame(width: 760).frame(maxWidth: .infinity, alignment: .leading).clipped()
        } else {
            content
        }
    }
}

/// Records main-run-loop cadence and process CPU around actual mouse clicks. The display link is not a
/// GPU frame counter: a long callback interval proves a main-thread stall, not a render-server hitch.
struct SidebarCadenceProbe: NSViewRepresentable {
    final class Probe: NSView {
        private var link: CADisplayLink?
        private var monitor: Any?
        private var previous: Double?
        private var ticks: [(Double, Double)] = []
        private var sequence = 0
        override func hitTest(_ point: NSPoint) -> NSView? { nil }
        override func viewDidMoveToWindow() {
            super.viewDidMoveToWindow()
            link?.invalidate()
            if let monitor { NSEvent.removeMonitor(monitor); self.monitor = nil }
            guard SidebarDiagnostics.enabled, let window else { return }
            link = window.displayLink(target: self, selector: #selector(tick(_:)))
            link?.add(to: .main, forMode: .common)
            monitor = NSEvent.addLocalMonitorForEvents(matching: .leftMouseDown) { [weak self] event in
                if let self, event.window === self.window { self.begin() }
                return event
            }
        }
        @objc private func tick(_ link: CADisplayLink) {
            let now = CACurrentMediaTime()
            if let previous { ticks.append((now, (now - previous) * 1000)) }
            previous = now
            ticks.removeAll { $0.0 < now - 5 }
        }
        private func cpu() -> Double {
            var usage = rusage()
            getrusage(RUSAGE_SELF, &usage)
            return Double(usage.ru_utime.tv_sec + usage.ru_stime.tv_sec) * 1000
                + Double(usage.ru_utime.tv_usec + usage.ru_stime.tv_usec) / 1000
        }
        private func begin() {
            sequence += 1
            let number = sequence, start = CACurrentMediaTime(), cpuStart = cpu()
            DispatchQueue.main.asyncAfter(deadline: .now() + 1) { [weak self] in
                guard let self, let path = SidebarDiagnostics.directory else { return }
                let gaps = self.ticks.filter { $0.0 >= start && $0.0 < start + 1 }.map(\.1).sorted()
                var visited = Set<ObjectIdentifier>(), glass = 0
                func layer(_ l: CALayer) {
                    guard visited.insert(ObjectIdentifier(l)).inserted else { return }
                    if String(describing: type(of: l)) == "CASDFLayer" { glass += 1 }
                    l.sublayers?.forEach(layer)
                }
                @MainActor func view(_ v: NSView) { if let l = v.layer { layer(l) }; v.subviews.forEach(view) }
                if let root = self.window?.contentView?.superview { view(root) }
                let result: [String: Any] = ["click": number, "surface": SidebarDiagnostics.surface,
                    "expanded": SidebarDiagnostics.expanded, "epoch": Date().timeIntervalSince1970,
                    "layout": ProcessInfo.processInfo.environment["MACCLUSTER_SIDEBAR_LAYOUT"] ?? "swiftui",
                    "detail": SidebarDiagnostics.detail, "toolbarHidden": SidebarDiagnostics.hideToolbar,
                    "cpuMS": self.cpu() - cpuStart, "maxCallbackGapMS": gaps.last ?? 0,
                    "callbacks": gaps.count, "gapsOver25MS": gaps.filter { $0 > 25 }.count,
                    "glassShapeLayers": glass]
                if let data = try? JSONSerialization.data(withJSONObject: result, options: [.sortedKeys]) {
                    let url = URL(fileURLWithPath: path).appendingPathComponent("click-\(number).json")
                    try? FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
                    try? data.write(to: url)
                }
            }
        }
    }
    func makeNSView(context: Context) -> Probe { Probe() }
    func updateNSView(_ view: Probe, context: Context) { }
}
