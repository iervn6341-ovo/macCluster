import SwiftUI
import AppKit

/// Keep the columns in one hosting hierarchy on macOS 27. NSSplitView's collapse animation repeatedly
/// asks its hosting views for minimum sizes and retiles the window toolbar while changing the columns.
/// A clipped, fixed-width sidebar avoids those AppKit/SwiftUI constraint round trips and retains state.
struct SidebarSplitView<Sidebar: View, Detail: View>: View {
    @Binding var visibility: NavigationSplitViewVisibility
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @AppStorage("sidebarWidth") private var storedWidth = 290.0
    @State private var dragStart: CGFloat?
    @ViewBuilder var sidebar: Sidebar
    @ViewBuilder var detail: Detail

    private var usesSwiftUI: Bool {
        if SidebarDiagnostics.enabled,
           ProcessInfo.processInfo.environment["MACCLUSTER_SIDEBAR_LAYOUT"] == "native" { return false }
        if #available(macOS 27, *) { return true }
        return false
    }
    private var expanded: Bool { visibility != .detailOnly }

    static func boundedWidth(_ width: CGFloat, available: CGFloat) -> CGFloat {
        min(max(240, width.isFinite ? width : 290), max(240, min(420, available - 560)))
    }

    var body: some View {
        if usesSwiftUI {
            GeometryReader { geometry in
                let width = Self.boundedWidth(storedWidth, available: geometry.size.width)
                HStack(spacing: 0) {
                    HStack(spacing: 0) {
                        sidebar.frame(width: width - 5)
                        divider(width: width, available: geometry.size.width)
                    }
                    .frame(width: width)
                    .frame(width: expanded ? width : 0, alignment: .leading)
                    .clipped()
                    .allowsHitTesting(expanded)
                    .disabled(!expanded)
                    .accessibilityHidden(!expanded)
                    detail.frame(maxWidth: .infinity, maxHeight: .infinity)
                }
                .frame(width: geometry.size.width, height: geometry.size.height)
                .animation(reduceMotion ? nil : .easeInOut(duration: 0.25), value: expanded)
            }
            .background { AppCanvas() }
            .navigationTitle("MacCluster")
            .toolbar {
                ToolbarItem(placement: .navigation) {
                    Button {
                        visibility = expanded ? .detailOnly : .all
                    } label: {
                        Label(expanded ? "收合側邊欄" : "展開側邊欄", systemImage: "sidebar.left")
                    }
                    .help(expanded ? "收合側邊欄" : "展開側邊欄")
                    .keyboardShortcut("s", modifiers: [.control, .command])
                    .accessibilityIdentifier("sidebar-toggle")
                }
            }
        } else {
            NavigationSplitView(columnVisibility: $visibility) { sidebar } detail: { detail }
        }
    }

    private func divider(width: CGFloat, available: CGFloat) -> some View {
        Rectangle().fill(.primary.opacity(0.001))
            .frame(width: 5)
            .contentShape(Rectangle())
            .onHover { if $0 { NSCursor.resizeLeftRight.set() } else { NSCursor.arrow.set() } }
            .gesture(DragGesture(minimumDistance: 0, coordinateSpace: .global)
                .onChanged { value in
                    if dragStart == nil { dragStart = width }
                    storedWidth = Self.boundedWidth((dragStart ?? width) + value.translation.width, available: available)
                }
                .onEnded { _ in dragStart = nil })
            .accessibilityElement()
            .accessibilityLabel("側邊欄寬度")
            .accessibilityValue("\(Int(width)) 點")
            .accessibilityAdjustableAction { direction in
                switch direction {
                case .increment: storedWidth = Self.boundedWidth(width + 20, available: available)
                case .decrement: storedWidth = Self.boundedWidth(width - 20, available: available)
                @unknown default: break
                }
            }
    }
}
