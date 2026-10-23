import SwiftUI
import AppKit

/// Pop-up screens float over the window instead of using a system sheet, following Launchbit's overlay:
/// a dim scrim over the content and a rounded panel on a within-window HUD material, forced dark, with a
/// hairline border and a soft shadow. A system sheet sits on a flat window material and `presentationBackground`
/// cannot remove it, so it can never look like this.

/// Closes the pop-up it is read from; the replacement for `@Environment(\.dismiss)` outside sheets.
struct ModalDismissAction: @unchecked Sendable {
    var perform: () -> Void = {}
    func callAsFunction() { perform() }
}

private struct ModalDismissKey: EnvironmentKey { static let defaultValue = ModalDismissAction() }

extension EnvironmentValues {
    var modalDismiss: ModalDismissAction {
        get { self[ModalDismissKey.self] }
        set { self[ModalDismissKey.self] = newValue }
    }
}

/// What the panel is made of. `.hud` is Launchbit's recipe; `.glass` draws a Liquid Glass effect instead.
enum ModalSurfaceStyle { case hud, glass }

/// A within-window HUD material, always dark like Launchbit's panels, so the controls on it stay legible
/// whatever the system appearance is.
private struct HUDMaterial: NSViewRepresentable {
    func makeNSView(context: Context) -> NSVisualEffectView {
        let view = NSVisualEffectView()
        view.material = .hudWindow
        view.blendingMode = .withinWindow
        view.state = .active
        view.appearance = NSAppearance(named: .darkAqua)
        return view
    }
    func updateNSView(_ view: NSVisualEffectView, context: Context) {}
}

private struct ModalSurface: ViewModifier {
    let style: ModalSurfaceStyle
    @Environment(\.accessibilityReduceTransparency) private var reduceTransparency
    private let radius: CGFloat = 24

    @ViewBuilder func body(content: Content) -> some View {
        if style == .glass, #available(macOS 26.0, *), !reduceTransparency {
            content.glassEffect(.regular, in: .rect(cornerRadius: radius))
                .shadow(color: .black.opacity(0.42), radius: 28, y: 8)
        } else {
            content
                .background { HUDMaterial().clipShape(RoundedRectangle(cornerRadius: radius)) }
                .overlay(RoundedRectangle(cornerRadius: radius).strokeBorder(.white.opacity(0.17), lineWidth: 1))
                .shadow(color: .black.opacity(0.42), radius: 28, y: 8)
        }
    }
}

struct ModalHost<Content: View>: View {
    /// What VoiceOver announces for the pop-up (Launchbit labels its overlay the same way).
    var label = ""
    /// Preferred size. The panel never exceeds the window minus a 24 pt margin on each side, and never shrinks below
    /// the minimums, as in Launchbit (settings: 640 x 704, at least 520 x 480).
    var width: CGFloat?
    /// Height for content that fills it (lists, scrolling text); nil keeps the content's own height.
    var height: CGFloat?
    var minWidth: CGFloat = 320
    var minHeight: CGFloat = 220
    /// False while an operation runs, so a click on the scrim cannot dismiss it half-way.
    var isDismissable = true
    var style: ModalSurfaceStyle = .hud
    let dismiss: () -> Void
    @ViewBuilder let content: Content

    var body: some View {
        GeometryReader { geo in
            let maxWidth = max(minWidth, geo.size.width - 48)
            let maxHeight = max(minHeight, geo.size.height - 48)
            ZStack {
                // A tap needs press and release on the scrim, so a drag that starts in the panel and ends
                // outside does not dismiss it.
                Color.black.opacity(0.28)
                    .ignoresSafeArea()
                    .contentShape(Rectangle())
                    .onTapGesture { if isDismissable { dismiss() } }
                    .accessibilityHidden(true)
                panel(maxWidth: maxWidth, maxHeight: maxHeight)
                    .environment(\.modalDismiss, ModalDismissAction(perform: dismiss))
                    .environment(\.colorScheme, .dark)
                    .modifier(ModalSurface(style: style))
                    .accessibilityElement(children: .contain)
                    .accessibilityLabel(label)
                    .accessibilityAddTraits(.isModal)
            }
            .frame(width: geo.size.width, height: geo.size.height)
        }
        .onExitCommand { if isDismissable { dismiss() } }
    }

    @ViewBuilder private func panel(maxWidth: CGFloat, maxHeight: CGFloat) -> some View {
        if let height {
            content.frame(width: width.map { min($0, maxWidth) }, height: min(height, maxHeight))
        } else {
            // Natural height; a window too small for it falls back to scrolling.
            NaturalModalLayout(maxHeight: maxHeight) {
                ViewThatFits(in: .vertical) {
                    content.fixedSize(horizontal: false, vertical: true)
                    ScrollView(.vertical) { content }
                }
            }
            .frame(width: width.map { min($0, maxWidth) }, alignment: .center)
        }
    }
}

/// Bound the proposal without stretching a short form to the window's height.
private struct NaturalModalLayout: Layout {
    let maxHeight: CGFloat

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        guard let content = subviews.first else { return .zero }
        let height = min(proposal.height ?? maxHeight, maxHeight)
        let size = content.sizeThatFits(ProposedViewSize(width: proposal.width, height: height))
        return CGSize(width: size.width, height: min(size.height, height))
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        subviews.first?.place(at: bounds.origin, anchor: .topLeading,
                             proposal: ProposedViewSize(width: bounds.width, height: bounds.height))
    }
}
