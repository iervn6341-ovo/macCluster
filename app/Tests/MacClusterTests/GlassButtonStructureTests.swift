import XCTest
import SwiftUI
@testable import MacCluster

/// Offscreen rendering does not draw Liquid Glass, but the hosting hierarchy shows whether a button got a
/// glass style: it adds SDF/backdrop layers that a bordered button does not. Counting them per screen
/// checks that the buttons on real screens are glass, without needing a screenshot.
/// Measured on macOS 26.6: a stand-alone glass button adds 2 CASDFLayers, a button inside a
/// GlassEffectContainer 1 (the container merges them), a bordered button 0. These are private
/// layer names, so a future OS may change the numbers; the intent is "glass buttons add layers".
@MainActor final class GlassButtonStructureTests: XCTestCase {
    private func pump(_ seconds: Double) {
        let end = Date().addingTimeInterval(seconds)
        while Date() < end { RunLoop.main.run(until: Date().addingTimeInterval(0.05)) }
    }

    /// Number of glass shape layers (one family per glass element) in a hosted view.
    private func glassLayers<V: View>(_ view: V, size: CGSize = CGSize(width: 700, height: 600)) -> Int {
        let host = NSHostingView(rootView: view.frame(width: size.width, height: size.height))
        host.frame = NSRect(origin: .zero, size: size)
        let window = NSWindow(contentRect: host.frame, styleMask: [.titled], backing: .buffered, defer: false)
        window.contentView = host
        host.layoutSubtreeIfNeeded()
        pump(0.8)
        var count = 0
        func walkLayer(_ layer: CALayer) {
            if String(describing: type(of: layer)) == "CASDFLayer" { count += 1 }
            (layer.sublayers ?? []).forEach(walkLayer)
        }
        func walk(_ view: NSView) {
            if let layer = view.layer { walkLayer(layer) }
            view.subviews.forEach(walk)
        }
        walk(host)
        window.contentView = nil
        return count
    }

    @available(macOS 26.0, *)
    func testButtonStylesAreDistinguishableByLayers() {
        XCTAssertEqual(glassLayers(Button("x") {}.buttonStyle(.bordered)), 0)
        XCTAssertEqual(glassLayers(Button("x") {}.buttonStyle(.glass)), 2)
        XCTAssertEqual(glassLayers(Button("x") {}.buttonStyle(.glassProminent)), 2)
        XCTAssertEqual(glassLayers(HStack { Button("a") {}; Button("b") {}; Button("c") {} }.buttonStyle(.glass)), 6)
    }

    @available(macOS 26.0, *)
    func testScreenModifiersGiveEveryButtonGlass() {
        // appScreen(): all plain buttons on a sheet.
        XCTAssertEqual(glassLayers(VStack { Button("取消") {}; Button("完成") {}; Button("重新讀取") {} }.appScreen()), 6)
        // glassControls(): an action row inside a GlassEffectContainer, one merged layer per button.
        XCTAssertEqual(glassLayers(HStack { Button("啟動") {}; Button("停止") {} }.glassControls()), 2)
        // A primary action stays glass (prominent).
        XCTAssertEqual(glassLayers(Button("儲存") {}.glassPrimaryAction()), 2)
        // The small inline copy button of a code block.
        XCTAssertEqual(glassLayers(Button("複製") {}.glassSmallButton()), 2)
    }

    @available(macOS 26.0, *)
    func testRealScreensHaveAGlassLayerPerButton() {
        let model = AppModel()
        // Environment form: 3 folder pickers, Cancel, Save.
        XCTAssertEqual(glassLayers(EnvironmentFormView(editing: nil).environmentObject(model), size: CGSize(width: 700, height: 700)), 10)
        // Code block header: the copy button.
        XCTAssertEqual(glassLayers(CodeBlock(title: "x.yaml", code: "a: b")), 2)
    }

    /// Pop-ups float in the window, never as system sheets (which sit on flat material). The default surface is
    /// Launchbit's within-window HUD material; the glass variant must draw glass layers.
    @available(macOS 26.0, *)
    func testModalPanelSurfaces() {
        func hudViews<V: View>(_ view: V) -> Int {
            let host = NSHostingView(rootView: view.frame(width: 700, height: 600))
            host.frame = NSRect(x: 0, y: 0, width: 700, height: 600)
            let window = NSWindow(contentRect: host.frame, styleMask: [.titled], backing: .buffered, defer: false)
            window.contentView = host
            host.layoutSubtreeIfNeeded()
            pump(0.8)
            var count = 0
            func walk(_ v: NSView) {
                if let effect = v as? NSVisualEffectView, effect.material == .hudWindow, effect.blendingMode == .withinWindow { count += 1 }
                v.subviews.forEach(walk)
            }
            walk(host)
            window.contentView = nil
            return count
        }
        let hud = ModalHost(label: "x", dismiss: {}) { Text("popup").padding(40) }
        XCTAssertEqual(hudViews(hud), 1, "the default panel is one within-window HUD material, like Launchbit's")
        XCTAssertEqual(hudViews(Text("plain").padding(40)), 0)
        XCTAssertGreaterThan(glassLayers(ModalHost(style: .glass, dismiss: {}) { Text("popup").padding(40) }), 0)
        XCTAssertEqual(glassLayers(ModalHost(style: .hud, dismiss: {}) { Text("popup").padding(40) }), 0, "the HUD surface adds no glass layers")
    }
}
