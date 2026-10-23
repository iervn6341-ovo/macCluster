import AppKit
import SwiftUI
import XCTest
@testable import MacCluster

@MainActor final class SidebarSplitViewTests: XCTestCase {
    private typealias Split = SidebarSplitView<Text, Text>

    func testSidebarWidthPreservesRoomForDetailAndRejectsInvalidValues() {
        XCTAssertEqual(Split.boundedWidth(100, available: 960), 240)
        XCTAssertEqual(Split.boundedWidth(1000, available: 960), 400)
        XCTAssertEqual(Split.boundedWidth(1000, available: 1400), 420)
        XCTAssertEqual(Split.boundedWidth(.nan, available: 960), 290)
    }

    private final class State: ObservableObject {
        @Published var visibility: NavigationSplitViewVisibility = .all
    }
    private struct Harness: View {
        @ObservedObject var state: State
        let sidebarView: NSView
        let detailView: NSView
        var body: some View {
            SidebarSplitView(visibility: $state.visibility) {
                Host(view: sidebarView)
            } detail: {
                Host(view: detailView)
            }
        }
    }
    private struct Host: NSViewRepresentable {
        let view: NSView
        func makeNSView(context: Context) -> NSView { view }
        func updateNSView(_ view: NSView, context: Context) { }
    }
    private func settle(_ seconds: Double = 0.4) {
        let end = Date().addingTimeInterval(seconds)
        while Date() < end { RunLoop.main.run(until: Date().addingTimeInterval(0.01)) }
    }

    func testCollapseKeepsViewsAliveAndRestoresDetailWidth() throws {
        guard #available(macOS 27, *) else { throw XCTSkip("macOS 27 split implementation") }
        let state = State(), sidebar = NSView(), detail = NSView()
        let host = NSHostingView(rootView: Harness(state: state, sidebarView: sidebar, detailView: detail))
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1100, height: 700),
                              styleMask: [.titled, .resizable], backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        window.contentView = host
        defer { window.contentView = nil; window.close() }
        host.frame = NSRect(x: 0, y: 0, width: 1100, height: 700)
        host.layoutSubtreeIfNeeded(); settle()
        let expandedWidth = detail.frame.width
        XCTAssertGreaterThan(expandedWidth, 500)
        state.visibility = .detailOnly
        settle(); host.layoutSubtreeIfNeeded()
        XCTAssertGreaterThan(detail.frame.width, expandedWidth + 200)
        XCTAssertTrue(sidebar.window === window, "collapsing must retain sidebar state")
        XCTAssertTrue(detail.window === window, "logs and terminal views must retain identity")
        state.visibility = .all
        settle(); host.layoutSubtreeIfNeeded()
        XCTAssertEqual(detail.frame.width, expandedWidth, accuracy: 1)
        XCTAssertGreaterThan(sidebar.frame.width, 230)
    }

    func testLongLogRewrapsWithoutReplacingItsDocumentWhenSidebarToggles() throws {
        guard #available(macOS 27, *) else { throw XCTSkip("macOS 27 split implementation") }
        let text = (1...10000).map { "line \($0): " + String(repeating: "long diagnostic log text ", count: 8) }.joined(separator: "\n")
        let logHost = NSHostingView(rootView: NativeLogView(text: text, identity: "stress", scrollAction: 0))
        logHost.sizingOptions = []
        let state = State()
        let host = NSHostingView(rootView: Harness(state: state, sidebarView: NSView(), detailView: logHost))
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1100, height: 700),
                              styleMask: [.titled, .resizable], backing: .buffered, defer: false)
        window.isReleasedWhenClosed = false
        window.contentView = host
        defer { window.contentView = nil; window.close() }
        host.frame = NSRect(x: 0, y: 0, width: 1100, height: 700)
        host.layoutSubtreeIfNeeded(); settle(0.8)
        func document(_ view: NSView) -> StripedLogTextView? {
            if let log = view as? StripedLogTextView { return log }
            for child in view.subviews { if let log = document(child) { return log } }
            return nil
        }
        let log = try XCTUnwrap(document(logHost))
        let storage = try XCTUnwrap(log.textStorage)
        let initialWidth = try XCTUnwrap(log.textContainer).containerSize.width
        XCTAssertEqual(storage.string, text)
        state.visibility = .detailOnly
        settle(0.8); host.layoutSubtreeIfNeeded(); settle(0.2)
        XCTAssertTrue(document(logHost) === log)
        XCTAssertTrue(log.textStorage === storage)
        XCTAssertGreaterThan(try XCTUnwrap(log.textContainer).containerSize.width, initialWidth + 200)
        XCTAssertEqual(log.textContainer?.containerSize.width ?? 0, log.frame.width - 24, accuracy: 1)
        state.visibility = .all
        settle(0.8); host.layoutSubtreeIfNeeded(); settle(0.2)
        XCTAssertEqual(log.textContainer?.containerSize.width ?? 0, initialWidth, accuracy: 1)
        XCTAssertEqual(storage.string, text)
    }

}
