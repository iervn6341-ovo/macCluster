import SwiftUI
import AppKit

/// A native log document that wraps to the viewport and scrolls vertically.
struct NativeLogView: NSViewRepresentable {
    @AppStorage("logFontSize") private var fontSize = 12.0
    @AppStorage("logLineSpacing") private var lineSpacing = 0.0
    @AppStorage("logCharacterSpacing") private var characterSpacing = 0.0
    let text: String
    let identity: String
    let scrollAction: Int
    var startsAtBottom = true

    final class Coordinator {
        var identity: String?
        var scrollAction = 0
        var source: String?
        var typography: [Double] = []
        var awaitingInitialLog = true
    }
    func makeCoordinator() -> Coordinator { Coordinator() }

    func makeNSView(context: Context) -> NSScrollView {
        let scroll = NSScrollView()
        scroll.hasVerticalScroller = true
        scroll.hasHorizontalScroller = false
        scroll.autohidesScrollers = false
        scroll.scrollerStyle = .legacy
        scroll.borderType = .bezelBorder
        scroll.drawsBackground = true
        scroll.backgroundColor = .textBackgroundColor

        let view = StripedLogTextView(frame: NSRect(x: 0, y: 0, width: max(1, scroll.contentSize.width), height: scroll.contentSize.height))
        view.isEditable = false
        view.isSelectable = true
        view.isRichText = false
        view.isAutomaticLinkDetectionEnabled = false
        view.font = .monospacedSystemFont(ofSize: 12, weight: .regular)
        view.textColor = .textColor
        view.backgroundColor = .textBackgroundColor
        view.textContainerInset = NSSize(width: 12, height: 12)
        view.minSize = .zero
        view.maxSize = NSSize(width: CGFloat.greatestFiniteMagnitude, height: CGFloat.greatestFiniteMagnitude)
        view.isVerticallyResizable = true
        view.isHorizontallyResizable = false
        view.autoresizingMask = [.width]
        view.textContainer?.widthTracksTextView = false // the width is applied by StripedLogTextView.rewrapNow()
        view.textContainer?.heightTracksTextView = false
        view.textContainer?.containerSize = NSSize(width: max(1, scroll.contentSize.width - 24), height: CGFloat.greatestFiniteMagnitude)
        view.setAccessibilityLabel("服務日誌")
        scroll.documentView = view
        return scroll
    }

    func updateNSView(_ scroll: NSScrollView, context: Context) {
        guard let view = scroll.documentView as? StripedLogTextView else { return }
        let switched = context.coordinator.identity != identity
        let oldOrigin = scroll.contentView.bounds.origin
        if switched { context.coordinator.awaitingInitialLog = true }
        let isLoading = text == "讀取中…"
        if isLoading { context.coordinator.awaitingInitialLog = true }
        let startAtBottom = context.coordinator.awaitingInitialLog && !isLoading
        let typography = [min(32, max(10, fontSize)), min(16, max(0, lineSpacing)), min(5, max(0, characterSpacing))]
        if switched || context.coordinator.source != text || context.coordinator.typography != typography {
            view.textStorage?.setAttributedString(ANSILogText.render(text, fontSize: typography[0], lineSpacing: typography[1], characterSpacing: typography[2]))
            view.updateLineStarts()
            context.coordinator.source = text
            context.coordinator.typography = typography
            view.setFrameSize(NSSize(width: scroll.contentSize.width, height: view.frame.height))
            view.rewrapNow()
            view.sizeToFit()
            let y = startAtBottom && startsAtBottom ? max(0, view.bounds.height - scroll.contentSize.height) : (switched ? 0 : oldOrigin.y)
            scroll.contentView.scroll(to: NSPoint(x: 0, y: y))
            if startAtBottom { context.coordinator.awaitingInitialLog = false }
            scroll.reflectScrolledClipView(scroll.contentView)
        }
        if context.coordinator.scrollAction != scrollAction {
            let y = scrollAction > 0 ? max(0, view.bounds.height - scroll.contentSize.height) : 0
            scroll.contentView.scroll(to: NSPoint(x: 0, y: y))
            scroll.reflectScrolledClipView(scroll.contentView)
        }
        context.coordinator.identity = identity
        context.coordinator.scrollAction = scrollAction
    }
}

/// Alternate logical log lines; wrapped fragments retain their line's shade.
final class StripedLogTextView: NSTextView {
    private var lineStarts = [0]
    private var rewrapWork: DispatchWorkItem?

    /// Makes the text container as wide as the view. Laying a long log out again at every width of an animation
    /// (the sidebar sliding in or out resizes this view on every frame) is what made that animation stutter, so
    /// while the view is being resized the wrapping is applied once, after the width has settled.
    func rewrapNow() {
        rewrapWork?.cancel()
        rewrapWork = nil
        let width = max(1, frame.width - textContainerInset.width * 2)
        if textContainer?.containerSize.width != width {
            textContainer?.containerSize = NSSize(width: width, height: CGFloat.greatestFiniteMagnitude)
        }
    }

    override func setFrameSize(_ newSize: NSSize) {
        let widthChanged = frame.width != newSize.width
        super.setFrameSize(newSize)
        guard widthChanged else { return }
        // Not on screen yet (first layout, offscreen rendering): wrap right away.
        guard window != nil else { rewrapNow(); return }
        rewrapWork?.cancel()
        let work = DispatchWorkItem { [weak self] in
            self?.rewrapNow()
            self?.sizeToFit()
        }
        rewrapWork = work
        DispatchQueue.main.asyncAfter(deadline: .now() + 0.15, execute: work)
    }

    func updateLineStarts() {
        lineStarts = [0]
        for (offset, unit) in string.utf16.enumerated() where unit == 10 {
            lineStarts.append(offset + 1)
        }
        needsDisplay = true
    }

    override func drawBackground(in rect: NSRect) {
        super.drawBackground(in: rect)
        guard let layout = layoutManager, let container = textContainer else { return }
        let origin = textContainerOrigin
        let textRect = rect.offsetBy(dx: -origin.x, dy: -origin.y)
        let glyphs = layout.glyphRange(forBoundingRect: textRect, in: container)
        layout.enumerateLineFragments(forGlyphRange: glyphs) { fragment, _, _, range, _ in
            let character = layout.characterIndexForGlyph(at: range.location)
            var low = 0
            var high = self.lineStarts.count
            while low < high {
                let middle = (low + high) / 2
                if self.lineStarts[middle] <= character { low = middle + 1 }
                else { high = middle }
            }
            guard max(0, low - 1) % 2 == 1 else { return }
            // Dynamic label color keeps the subtle stripe visible in both themes.
            NSColor.labelColor.withAlphaComponent(0.065).setFill()
            NSRect(x: self.bounds.minX, y: fragment.minY + origin.y,
                   width: self.bounds.width, height: fragment.height)
                .intersection(rect).fill()
        }
    }
}

/// One rule for how tall the log area is, so every page shows the same amount of log
/// whatever sits above it. The page scrolls when the content above does not fit.
enum LogLayout {
    /// The log never shows fewer lines than this, however small the window is.
    static let minimumVisibleLines = 20
    /// Room kept for the page header when the log grows with a tall window.
    static let reservedAboveLog: CGFloat = 120
    /// Text container inset (12 top and bottom) plus the bezel.
    private static let chrome: CGFloat = 12 * 2 + 4

    static func lineHeight(fontSize: Double, lineSpacing: Double) -> CGFloat {
        let font = NSFont.monospacedSystemFont(ofSize: fontSize, weight: .regular)
        return NSLayoutManager().defaultLineHeight(for: font) + CGFloat(lineSpacing)
    }

    static func minimumHeight(fontSize: Double, lineSpacing: Double) -> CGFloat {
        CGFloat(minimumVisibleLines) * lineHeight(fontSize: fontSize, lineSpacing: lineSpacing) + chrome
    }

    /// Identical on every page for a given window; `viewport` is the height of the scrolling page.
    static func height(viewport: CGFloat, fontSize: Double, lineSpacing: Double) -> CGFloat {
        max(minimumHeight(fontSize: fontSize, lineSpacing: lineSpacing), viewport - reservedAboveLog)
    }
}

struct LogPanel: View {
    @AppStorage("logFontSize") private var fontSize = 12.0
    @AppStorage("logLineSpacing") private var lineSpacing = 0.0
    let viewport: CGFloat
    let text: String
    let identity: String
    let scrollAction: Int

    var body: some View {
        NativeLogView(text: text, identity: identity, scrollAction: scrollAction)
            .frame(maxWidth: .infinity)
            .frame(height: LogLayout.height(viewport: viewport, fontSize: fontSize, lineSpacing: lineSpacing))
    }
}
