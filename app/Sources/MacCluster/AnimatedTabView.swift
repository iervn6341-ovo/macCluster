import SwiftUI

/// Tabs in plain SwiftUI: the same bar-and-pages layout as the system tab control, but nothing in it depends on how
/// a given macOS release sizes an NSTabView, and the animation is ours: the selection indicator slides to the
/// chosen tab and the page slides in from the direction of travel while the old one fades out.
struct AnimatedTabView: View {
    struct Page: Identifiable {
        let id: String
        let title: String
        let content: AnyView
    }

    let pages: [Page]
    @Binding var selection: String
    /// Narrower bar for few tabs; nil spreads the tabs over the full width.
    var barMaxWidth: CGFloat?

    @Namespace private var indicator
    /// +1 when the new page is to the right of the old one, -1 when to the left.
    @State private var direction: CGFloat = 1
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private var animation: Animation? { reduceMotion ? nil : .snappy(duration: 0.28) }
    private var current: Page? { pages.first { $0.id == selection } ?? pages.first }

    private func select(_ id: String) {
        guard id != selection,
              let new = pages.firstIndex(where: { $0.id == id }),
              let old = pages.firstIndex(where: { $0.id == selection }) else { return }
        direction = new > old ? 1 : -1
        withAnimation(animation) { selection = id }
    }

    /// The page enters from the side it is on and fades in; the old page only fades out. A removal keeps the
    /// transition it was last drawn with, so a direction-dependent removal would slide the wrong way.
    private var pageTransition: AnyTransition {
        reduceMotion ? .identity : .asymmetric(
            insertion: .modifier(active: SlideFade(offset: 36 * direction, opacity: 0), identity: SlideFade(offset: 0, opacity: 1)),
            removal: .opacity)
    }

    var body: some View {
        VStack(spacing: 14) {
            bar
            ZStack {
                if let page = current {
                    ScrollView(.vertical) {
                        page.content
                            .padding(22)
                            .frame(maxWidth: .infinity, alignment: .topLeading)
                    }
                    .id(page.id)
                    .transition(pageTransition)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .clipped()
            .glassButtons()
            .groupBoxStyle(SheetGroupBoxStyle())
        }
    }

    private var bar: some View {
        HStack(spacing: 2) {
            ForEach(pages) { page in
                let selected = page.id == selection
                Button { select(page.id) } label: {
                    Text(page.title)
                        .font(.system(size: 13, weight: selected ? .semibold : .regular))
                        .lineLimit(1)
                        .frame(maxWidth: .infinity)
                        .padding(.vertical, 6)
                        .contentShape(Capsule())
                }
                .buttonStyle(.plain)
                .foregroundStyle(selected ? Color.primary : Color.secondary)
                .background {
                    if selected {
                        Capsule().fill(.white.opacity(0.16))
                            .overlay(Capsule().strokeBorder(.white.opacity(0.24), lineWidth: 0.5))
                            .matchedGeometryEffect(id: "tab-indicator", in: indicator)
                    }
                }
                .accessibilityAddTraits(selected ? .isSelected : [])
                .accessibilityLabel(page.title)
            }
        }
        .padding(3)
        .background(.white.opacity(0.07), in: Capsule())
        .overlay(Capsule().strokeBorder(.white.opacity(0.10), lineWidth: 0.5))
        .frame(maxWidth: barMaxWidth)
        .animation(animation, value: selection)
        .accessibilityElement(children: .contain)
        .accessibilityLabel("分頁")
    }
}

private struct SlideFade: ViewModifier {
    let offset: CGFloat
    let opacity: Double
    func body(content: Content) -> some View { content.offset(x: offset).opacity(opacity) }
}
