import AppKit
import SwiftUI

/// A non-activating tooltip window avoids clipping inside the native toolbar.
@MainActor private final class ToolbarTipController: ObservableObject {
    weak var anchor: NSView?
    private var panel: NSPanel?

    func show(_ text: String) {
        hide()
        guard let anchor, let window = anchor.window else { return }
        let label = NSTextField(labelWithString: text)
        label.font = .systemFont(ofSize: 12)
        label.textColor = .labelColor
        label.sizeToFit()
        let size = NSSize(width: label.frame.width + 20, height: label.frame.height + 14)
        let tip = NSPanel(contentRect: NSRect(origin: .zero, size: size), styleMask: [.borderless, .nonactivatingPanel], backing: .buffered, defer: false)
        tip.isOpaque = false
        tip.backgroundColor = .clear
        tip.hasShadow = true
        tip.ignoresMouseEvents = true
        tip.hidesOnDeactivate = true
        let background = NSVisualEffectView(frame: NSRect(origin: .zero, size: size))
        background.material = .popover
        background.state = .active
        background.wantsLayer = true
        background.layer?.cornerRadius = 7
        background.layer?.masksToBounds = true
        label.setFrameOrigin(NSPoint(x: 10, y: 7))
        background.addSubview(label)
        tip.contentView = background
        let rect = window.convertToScreen(anchor.convert(anchor.bounds, to: nil))
        let screen = window.screen?.visibleFrame ?? rect
        let x = min(max(rect.midX - size.width / 2, screen.minX + 6), screen.maxX - size.width - 6)
        tip.setFrameOrigin(NSPoint(x: x, y: rect.minY - size.height - 8))
        window.addChildWindow(tip, ordered: .above)
        tip.orderFront(nil)
        panel = tip
    }

    func hide() {
        guard let panel else { return }
        panel.parent?.removeChildWindow(panel)
        panel.orderOut(nil)
        self.panel = nil
    }
}

private final class TooltipTrackingView: NSView {
    weak var controller: ToolbarTipController?
    var text = ""
    override func hitTest(_ point: NSPoint) -> NSView? { nil }
    override func updateTrackingAreas() {
        super.updateTrackingAreas()
        trackingAreas.forEach(removeTrackingArea)
        addTrackingArea(NSTrackingArea(rect: .zero,
            options: [.mouseEnteredAndExited, .activeAlways, .inVisibleRect],
            owner: self, userInfo: nil))
    }
    override func mouseEntered(with event: NSEvent) { controller?.show(text) }
    override func mouseExited(with event: NSEvent) { controller?.hide() }
}

private struct TooltipAnchor: NSViewRepresentable {
    let controller: ToolbarTipController
    let text: String
    func makeNSView(context: Context) -> TooltipTrackingView {
        let view = TooltipTrackingView()
        view.controller = controller
        view.text = text
        controller.anchor = view
        return view
    }
    func updateNSView(_ nsView: TooltipTrackingView, context: Context) {
        controller.anchor = nsView
        nsView.text = text
    }
    static func dismantleNSView(_ nsView: TooltipTrackingView, coordinator: ()) {
        nsView.controller?.hide()
    }
}

private struct ToolbarTooltip: ViewModifier {
    let text: String
    @StateObject private var controller = ToolbarTipController()
    func body(content: Content) -> some View {
        content
            .background(TooltipAnchor(controller: controller, text: text))
            .simultaneousGesture(TapGesture().onEnded { controller.hide() })
            .onDisappear { controller.hide() }
            .onReceive(NotificationCenter.default.publisher(for: NSWindow.didResignKeyNotification)) { _ in controller.hide() }
    }
}

extension View {
    func toolbarTooltip(_ text: String) -> some View { modifier(ToolbarTooltip(text: text)) }
}
