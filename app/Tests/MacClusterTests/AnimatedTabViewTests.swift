import AppKit
import SwiftUI
import XCTest
@testable import MacCluster

@MainActor
final class AnimatedTabViewTests: XCTestCase {
    private final class Box: ObservableObject { @Published var tab = "a"; @Published var shown: [String] = [] }

    private struct Host: View {
        @ObservedObject var box: Box
        var body: some View {
            AnimatedTabView(pages: ["a", "b", "c"].map { id in
                .init(id: id, title: id.uppercased(), content: AnyView(Text(id).onAppear { box.shown.append(id) }))
            }, selection: $box.tab, barMaxWidth: 300)
            .frame(width: 600, height: 400)
        }
    }

    func testOnlySelectedPageIsShownAndSelectionSwitchesIt() async throws {
        let box = Box()
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 600, height: 400), styleMask: [.titled], backing: .buffered, defer: false)
        window.contentView = NSHostingView(rootView: Host(box: box))
        window.orderBack(nil)
        try await Task.sleep(nanoseconds: 400_000_000)
        XCTAssertEqual(box.shown, ["a"])
        box.tab = "c"
        try await Task.sleep(nanoseconds: 600_000_000)
        XCTAssertEqual(box.shown, ["a", "c"], "the page that was not selected must not be created")
        window.orderOut(nil)
    }
}
