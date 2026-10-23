import SwiftUI
import AppKit

/// Keep glass in the controls layer; logs and inspection text retain solid backgrounds.
struct GlassControls: ViewModifier {
    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency

    @ViewBuilder func body(content: Content) -> some View {
        if #available(macOS 26.0, *), !reduceTransparency {
            GlassEffectContainer(spacing: 12) {
                content.buttonStyle(.glass)
            }
        } else {
            content.buttonStyle(.bordered)
        }
    }
}

extension View {
    func glassControls() -> some View { modifier(GlassControls()) }
}

/// Sample the desktop behind the window instead of covering it with an opaque fill.
private struct WindowBackdrop: NSViewRepresentable {
    final class BackdropView: NSVisualEffectView {
        override func viewDidMoveToWindow() {
            super.viewDidMoveToWindow()
            window?.isOpaque = false
            window?.backgroundColor = .clear
        }
        override func hitTest(_ point: NSPoint) -> NSView? { nil }
    }
    func makeNSView(context: Context) -> BackdropView {
        let view = BackdropView()
        view.material = .sidebar
        view.blendingMode = .behindWindow
        view.state = .active
        return view
    }
    func updateNSView(_ nsView: BackdropView, context: Context) { }
}

struct AppCanvas: View {
    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency
    @Environment(\.colorScheme) private var colorScheme
    var body: some View {
        Group {
            if reduceTransparency {
                Color(nsColor: .windowBackgroundColor)
            } else {
                WindowBackdrop()
                    .overlay(colorScheme == .dark ? Color.black.opacity(0.18) : Color.white.opacity(0.08))
            }
        }
        .ignoresSafeArea()
        .allowsHitTesting(false)
    }
}

extension View {
    /// The sidebar's own backdrop. Before macOS 27 the system draws the sidebar as a floating glass panel and the
    /// canvas goes behind it. On 27 the sidebar comes out flat and edge to edge (the panel's glass is not drawn), so
    /// the panel is drawn here: an inset rounded Liquid Glass shape, which is what the earlier systems show.
    @ViewBuilder func sidebarCanvas() -> some View {
        if #available(macOS 27, *) {
            // `defaults write com.maccluster.desktop sidebarGlass -bool false` swaps the Liquid Glass for a plain
            // material, to tell whether the glass is what makes the sidebar hitch when it is shown again.
            if SidebarDiagnostics.enabled ? SidebarDiagnostics.surface == "glass" : (UserDefaults.standard.object(forKey: "sidebarGlass") as? Bool ?? true) {
                self
                    .glassEffect(.regular, in: .rect(cornerRadius: 26))
                    .padding(.horizontal, 8)
                    .padding(.bottom, 8)
            } else {
                self
                    .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 26))
                    .overlay(RoundedRectangle(cornerRadius: 26).strokeBorder(.primary.opacity(0.12), lineWidth: 0.5))
                    .padding(.horizontal, 8)
                    .padding(.bottom, 8)
            }
        } else {
            self.background { AppCanvas() }
        }
    }
}

/// Content (cards, headings, settings groups) lives below the controls layer, so it
/// uses a material, not Liquid Glass. Glass is reserved for controls and navigation.
private struct ContentCard: ViewModifier {
    func body(content: Content) -> some View {
        content
            .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 12))
            .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(.primary.opacity(0.10), lineWidth: 0.5))
    }
}

/// Buttons on a screen use the Liquid Glass style. Content stays on materials, so glass is only ever
/// the controls layer. Without macOS 26 or with Reduce Transparency the system bordered style is used.
private struct GlassButtons: ViewModifier {
    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency
    @ViewBuilder func body(content: Content) -> some View {
        if #available(macOS 26.0, *), !reduceTransparency {
            content.buttonStyle(.glass)
        } else {
            content.buttonStyle(.bordered)
        }
    }
}

/// The look of a pop-up's content (the glass panel itself comes from ModalHost): glass buttons and translucent panels. Action rows that
/// sit together use glassControls() (a GlassEffectContainer), primary actions glassPrimaryAction().
private struct AppScreen: ViewModifier {
    func body(content: Content) -> some View {
        content.modifier(GlassButtons()).groupBoxStyle(SheetGroupBoxStyle())
    }
}

/// Panels inside a sheet are translucent tints: the sheet is the glass, and an opaque card would hide it.
private struct SheetPanel: ViewModifier {
    func body(content: Content) -> some View {
        content
            .background(.primary.opacity(0.06), in: RoundedRectangle(cornerRadius: 14))
            .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(.primary.opacity(0.14), lineWidth: 0.5))
    }
}

/// A small inline action, such as "copy" in a code block header.
private struct GlassSmallButton: ViewModifier {
    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency
    @ViewBuilder func body(content: Content) -> some View {
        if #available(macOS 26.0, *), !reduceTransparency {
            content.buttonStyle(.glass).controlSize(.small)
        } else {
            content.buttonStyle(.bordered).controlSize(.small)
        }
    }
}

extension View {
    /// Glass buttons for everything below (system bordered style without macOS 26 or with Reduce Transparency).
    func glassButtons() -> some View { modifier(GlassButtons()) }
    func sheetPanel() -> some View { modifier(SheetPanel()) }
    func glassSmallButton() -> some View { modifier(GlassSmallButton()) }
    func contentCard() -> some View { modifier(ContentCard()) }
    func appScreen() -> some View { modifier(AppScreen()) }
}

private struct GlassPrimaryAction: ViewModifier {
    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency
    @ViewBuilder func body(content: Content) -> some View {
        if #available(macOS 26.0, *), !reduceTransparency {
            content.buttonStyle(.glassProminent)
        } else {
            content.buttonStyle(.borderedProminent)
        }
    }
}

extension View {
    func glassPrimaryAction() -> some View { modifier(GlassPrimaryAction()) }
    /// A sheet title: plain text on the sheet, no panel behind it.
    func sheetHeading() -> some View {
        frame(maxWidth: .infinity, alignment: .leading).padding(.bottom, 4)
    }
}

struct SidebarIconLabelStyle: LabelStyle {
    func makeBody(configuration: Configuration) -> some View {
        HStack(spacing: 9) {
            configuration.icon.font(.system(size: 17, weight: .medium)).frame(width: 22)
            configuration.title
        }
    }
}

/// Neutral selection for sidebar rows. It is a flat fill, not glass: the sidebar is
/// already a glass surface and glass cannot sample other glass.
struct SelectionHighlight: View {
    var body: some View {
        RoundedRectangle(cornerRadius: 8).fill(.primary.opacity(0.14))
            .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(.primary.opacity(0.18), lineWidth: 0.5))
    }
}

private struct SidebarHover: ViewModifier {
    @State private var hovered = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    func body(content: Content) -> some View {
        content
            .background(.primary.opacity(hovered ? 0.065 : 0), in: RoundedRectangle(cornerRadius: 8))
            .overlay(RoundedRectangle(cornerRadius: 8)
                .strokeBorder(.primary.opacity(hovered ? 0.12 : 0), lineWidth: 0.5)
                .allowsHitTesting(false))
            .onHover { hovered = $0 }
            .animation(reduceMotion ? nil : .easeOut(duration: 0.15), value: hovered)
    }
}

extension View {
    func sidebarHover() -> some View { modifier(SidebarHover()) }
}

/// Render disclosure content within the row so both the arrow and programmatic
/// expansion use SwiftUI's animation, instead of separate native outline paths.
struct SidebarDisclosureStyle: DisclosureGroupStyle {
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    func makeBody(configuration: Configuration) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 2) {
                Button {
                    configuration.isExpanded.toggle()
                } label: {
                    Image(systemName: "chevron.right")
                        .font(.system(size: 10, weight: .semibold))
                        .rotationEffect(.degrees(configuration.isExpanded ? 90 : 0))
                        .frame(width: 20, height: 28)
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel(configuration.isExpanded ? "收合" : "展開")
                configuration.label
            }
            // Keep the clipping viewport below the header while its contents move.
            VStack(alignment: .leading, spacing: 0) {
                if configuration.isExpanded {
                    VStack(alignment: .leading, spacing: 4) {
                        configuration.content
                    }
                    .padding(.leading, 22)
                    .padding(.top, 4)
                    .transition(.opacity.combined(with: .move(edge: .top)))
                }
            }
            .clipped()
        }
        .clipped()
        .animation(reduceMotion ? nil : .easeInOut(duration: 0.25), value: configuration.isExpanded)
        .accessibilityElement(children: .contain)
    }
}

/// Group boxes are content cards.
struct CardGroupBoxStyle: GroupBoxStyle {
    func makeBody(configuration: Configuration) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            configuration.label.font(.headline)
            configuration.content
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(14)
        .contentCard()
    }
}

/// Group boxes inside a sheet.
struct SheetGroupBoxStyle: GroupBoxStyle {
    func makeBody(configuration: Configuration) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            configuration.label.font(.headline)
            configuration.content
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(14)
        .sheetPanel()
    }
}
