import AppKit
import SwiftUI
import ScreenCaptureKit

/// Development aid, inert unless MACCLUSTER_DEV_SNAPSHOT_DIR is set. SIGUSR2 writes layers.txt: the view
/// and layer structure of every visible window (toolbar and sheets included) with the glass layers marked,
/// which needs no Screen Recording permission. SIGUSR1 writes
/// a PNG of each visible window of this app (sheets included) to that directory, so
/// real Liquid Glass rendering can be inspected without Screen Recording for a shell.
@MainActor enum DevSnapshot {
    private static var source: DispatchSourceSignal?
    private static var layerSource: DispatchSourceSignal?

    static func installIfRequested() {
        guard let path = ProcessInfo.processInfo.environment["MACCLUSTER_DEV_SNAPSHOT_DIR"], !path.isEmpty else { return }
        let directory = URL(fileURLWithPath: path, isDirectory: true)
        try? FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        signal(SIGUSR1, SIG_IGN)
        let source = DispatchSource.makeSignalSource(signal: SIGUSR1, queue: .main)
        source.setEventHandler { Task { @MainActor in await capture(to: directory) } }
        source.resume()
        self.source = source
        signal(SIGUSR2, SIG_IGN)
        let layerSource = DispatchSource.makeSignalSource(signal: SIGUSR2, queue: .main)
        layerSource.setEventHandler { Task { @MainActor in dumpStructure(to: directory) } }
        layerSource.resume()
        self.layerSource = layerSource
    }

    private static var architecture: String {
        var info = utsname(); uname(&info)
        return withUnsafePointer(to: &info.machine) { $0.withMemoryRebound(to: CChar.self, capacity: 1) { String(cString: $0) } }
    }

    private static func isGlass(_ layer: CALayer) -> Bool {
        let name = String(describing: type(of: layer))
        return name == "CASDFLayer" || name == "CABackdropLayer"
    }

    private static func glassCount(_ layer: CALayer) -> Int {
        (isGlass(layer) ? 1 : 0) + (layer.sublayers ?? []).reduce(0) { $0 + glassCount($1) }
    }

    private static func dumpStructure(to directory: URL) {
        var out = "OS \(ProcessInfo.processInfo.operatingSystemVersionString) arch=\(Self.architecture)\n"
        for window in NSApp.windows where window.isVisible {
            out += "WINDOW \(type(of: window)) title=\(window.title.debugDescription) frame=\(window.frame) opaque=\(window.isOpaque) background=\(String(describing: window.backgroundColor)) style=\(window.styleMask.rawValue) sheet=\(window.isSheet) toolbar=\(window.toolbar != nil) toolbarStyle=\(window.toolbarStyle.rawValue)\n"
            guard let root = window.contentView?.superview ?? window.contentView else { continue }
            func walk(_ view: NSView, _ depth: Int) {
                let glass = view.layer.map(glassCount) ?? 0
                let name = String(describing: type(of: view))
                let short = name.count > 90 ? String(name.prefix(90)) + "…" : name
                var extra = ""
                if let tabView = view as? NSTabView { extra += " tabs=\(tabView.tabViewItems.map(\.label)) selected=\(tabView.selectedTabViewItem?.label ?? "-") type=\(tabView.tabViewType.rawValue) frame=\(tabView.frame)" }
                if view is NSVisualEffectView { extra += " frame=\(view.frame)" }
                if let tabView = view as? NSTabView {
                    extra += " contentRect=\(tabView.contentRect) tabViewBorderType=\(tabView.tabViewBorderType.rawValue) safeArea=\(view.safeAreaInsets)"
                }
                if name.hasPrefix("NSHostingView") || name.hasPrefix("_NSHostingView") {
                    extra += " frame=\(view.frame) safeArea=\(view.safeAreaInsets)"
                    if #available(macOS 14.0, *), let host = view as? NSHostingView<AnyView> { extra += " sizingOptions=\(host.sizingOptions.rawValue) safeAreaRegions=\(host.safeAreaRegions.rawValue)" }
                }
                if let effect = view as? NSVisualEffectView { extra = " material=\(effect.material.rawValue) blending=\(effect.blendingMode.rawValue) state=\(effect.state.rawValue) appearance=\(effect.appearance?.name.rawValue ?? "inherited")" }
                out += String(repeating: "  ", count: depth) + short + extra + (glass > 0 ? "  [glass layers in subtree: \(glass)]" : "") + (view.isHidden ? " (hidden)" : "") + "\n"
                if depth < 14 { view.subviews.forEach { walk($0, depth + 1) } }
            }
            walk(root, 1)
            // Checks for what the tab pop-ups assume: every tab page fills the tab view's content area.
            func tabViews(_ v: NSView) -> [NSTabView] { (v as? NSTabView).map { [$0] } ?? v.subviews.flatMap(tabViews) }
            for tabView in tabViews(root) {
                // NSTabView only lays out the selected page; the others keep a zero frame until selected.
                for item in [tabView.selectedTabViewItem].compactMap({ $0 }) {
                    guard let page = item.view else { continue }
                    let fills = page.frame.equalTo(tabView.contentRect)
                    out += "CHECK tab \(item.label.debugDescription): page frame \(page.frame) vs contentRect \(tabView.contentRect) -> \(fills ? "ok" : "MISMATCH")\n"
                }
            }
        }
        try? out.write(to: directory.appendingPathComponent("layers.txt"), atomically: true, encoding: .utf8)
        // The window's own views drawn in-process (no screen-recording permission needed). Glass and
        // behind-window materials are not drawn this way, but text and controls are, which is what shows
        // whether a page rendered at all.
        for (index, window) in NSApp.windows.enumerated() {
            guard let content = window.contentView?.superview ?? window.contentView,
                  let rep = content.bitmapImageRepForCachingDisplay(in: content.bounds) else { continue }
            content.cacheDisplay(in: content.bounds, to: rep)
            if let png = rep.representation(using: .png, properties: [:]) {
                try? png.write(to: directory.appendingPathComponent("window\(index).png"))
            }
        }
    }

    private static func capture(to directory: URL) async {
        let stamp = Int(Date().timeIntervalSince1970)
        do {
            let content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: true)
            let mine = Set(NSApp.windows.filter(\.isVisible).map { CGWindowID($0.windowNumber) })
            var index = 0
            for window in content.windows where mine.contains(window.windowID) && window.frame.width > 80 {
                let configuration = SCStreamConfiguration()
                configuration.width = Int(window.frame.width * 2)
                configuration.height = Int(window.frame.height * 2)
                configuration.showsCursor = false
                let image = try await SCScreenshotManager.captureImage(contentFilter: SCContentFilter(desktopIndependentWindow: window), configuration: configuration)
                let rep = NSBitmapImageRep(cgImage: image)
                if let data = rep.representation(using: .png, properties: [:]) {
                    try data.write(to: directory.appendingPathComponent("snap-\(stamp)-\(index).png"))
                }
                index += 1
            }
            try "ok \(index) windows".write(to: directory.appendingPathComponent("last.txt"), atomically: true, encoding: .utf8)
        } catch {
            try? "error \(error)".write(to: directory.appendingPathComponent("last.txt"), atomically: true, encoding: .utf8)
        }
    }
}
