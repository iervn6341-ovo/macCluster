import SwiftUI

/// A card of the detail pages that can be folded. Whether it is open is remembered per kind of card (not per
/// workload), so a person who folds "連接埠" once sees it folded on every service until they open it again.
struct CollapsibleCard<Content: View>: View {
    let title: String
    let systemImage: String
    // A local @State drives the view so that toggling can be animated; an @AppStorage value changes outside the
    // animation transaction and the card would jump. The choice is written back to the preference.
    @State private var expanded: Bool
    private let storageKey: String
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @ViewBuilder let content: Content

    init(_ title: String, systemImage: String, storageKey: String, @ViewBuilder content: () -> Content) {
        self.title = title
        self.systemImage = systemImage
        let key = "card.expanded." + storageKey
        self.storageKey = key
        _expanded = State(initialValue: UserDefaults.standard.object(forKey: key) as? Bool ?? true)
        self.content = content()
    }

    var body: some View {
        GroupBox {
            if expanded {
                content
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.top, 4)
                    .clipped() // never draw over the title while the card is folding
                    .transition(reduceMotion ? .identity : .opacity)
            }
        } label: {
            Button {
                withAnimation(reduceMotion ? nil : .easeInOut(duration: 0.25)) { expanded.toggle() }
                UserDefaults.standard.set(expanded, forKey: storageKey)
            } label: {
                HStack(spacing: 6) {
                    Image(systemName: "chevron.right")
                        .font(.caption.weight(.semibold))
                        .rotationEffect(.degrees(expanded ? 90 : 0))
                        .frame(width: 12)
                    Label(title, systemImage: systemImage)
                    Spacer(minLength: 0)
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel(title + (expanded ? "，已展開" : "，已折疊"))
            .accessibilityHint("切換展開或折疊")
        }
    }
}
