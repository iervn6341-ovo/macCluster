import SwiftUI
import AppKit

struct KubernetesEvent: Decodable, Identifiable, Sendable {
    let id: String
    let time: String
    let clusterID: String
    let clusterName: String
    let namespace: String
    let workload: String
    let kind: String
    let level: String?
    let message: String
}

struct KubernetesEventRow: View {
    let event: KubernetesEvent
    private var timestamp: String {
        let parser = ISO8601DateFormatter()
        parser.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let date = parser.date(from: event.time) ?? ISO8601DateFormatter().date(from: event.time)
        return date?.formatted(date: .abbreviated, time: .standard) ?? event.time
    }
    private var level: String {
        ["info", "warning", "error"].contains(event.level ?? "") ? event.level! : "info"
    }
    private var levelColor: Color {
        // ANSI normal green/yellow/red, matching ANSILogText's palette.
        let rgb: UInt32 = level == "error" ? 0xCD3131 : level == "warning" ? 0xE5B50A : 0x0DBC79
        return Color(red: Double((rgb >> 16) & 255) / 255,
                     green: Double((rgb >> 8) & 255) / 255, blue: Double(rgb & 255) / 255)
    }
    var body: some View {
        GroupBox {
            VStack(alignment: .leading, spacing: 6) {
                HStack(alignment: .firstTextBaseline) {
                    Text(level.uppercased()).font(.caption.monospaced().bold())
                        .foregroundStyle(levelColor)
                        .padding(.horizontal, 6).padding(.vertical, 3)
                        .background(levelColor.opacity(0.12), in: RoundedRectangle(cornerRadius: 4))
                    Text(event.message).font(.headline).foregroundStyle(levelColor)
                    Spacer(minLength: 12)
                    Text(timestamp).font(.caption).foregroundStyle(.secondary)
                }
                Text([event.clusterName.isEmpty ? event.clusterID : event.clusterName,
                      event.namespace, event.workload].filter { !$0.isEmpty }.joined(separator: " · "))
                    .font(.caption).foregroundStyle(.secondary)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .textSelection(.enabled)
        }
    }
}

struct KubernetesIcon: View {
    // Official CNCF white icon, bundled without recoloring its geometry.
    @MainActor private static let artwork: NSImage? = {
        guard let url = Bundle.main.url(forResource: "KubernetesIconWhite", withExtension: "png") else { return nil }
        return NSImage(contentsOf: url)
    }()
    var body: some View {
        if let artwork = Self.artwork {
            Image(nsImage: artwork).resizable().renderingMode(.original)
                .scaledToFit().frame(width: 20, height: 20).accessibilityHidden(true)
        }
    }
}
