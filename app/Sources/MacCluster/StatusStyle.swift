import SwiftUI

/// One place that decides what colour a status gets, so the sidebar, detail pages, clusters
/// and Compose projects agree. Colour is never the only signal: a dot and the status text
/// always come with it.
enum StatusTone: Equatable {
    case running        // working as intended
    case transitioning  // on its way somewhere: starting, joining, restarting, paused
    case stopped        // deliberately not running, finished cleanly
    case failed         // needs attention
    case unknown        // cannot be confirmed (agent or Docker not answering)

    var color: Color {
        switch self {
        case .running: return .green
        case .transitioning: return .orange
        case .stopped: return .gray
        case .failed: return .red
        case .unknown: return .purple
        }
    }
    var label: String {
        switch self {
        case .running: return "運行中"
        case .transitioning: return "進行中"
        case .stopped: return "已停止"
        case .failed: return "異常"
        case .unknown: return "無法確認"
        }
    }
}

enum StatusStyle {
    /// Workload, Pod, container and cluster phases the agent and Docker report.
    static func tone(phase: String) -> StatusTone {
        switch phase {
        case "Running", "Ready":
            return .running
        case "Pending", "Joining", "Leaving", "Restarting", "Paused", "Removing":
            return .transitioning
        case "Succeeded", "Stopped", "Created":
            return .stopped
        case "Failed", "Error", "Dead", "Removed":
            return .failed
        default:
            return .unknown
        }
    }

    /// A Compose project is only green when every container runs.
    static func tone(compose phases: [String]) -> StatusTone {
        let tones = phases.map { tone(phase: $0) }
        if tones.isEmpty { return .unknown }
        if tones.contains(.failed) { return .failed }
        if tones.allSatisfy({ $0 == .running }) { return .running }
        if tones.contains(.unknown) { return .unknown }
        if tones.contains(.running) || tones.contains(.transitioning) { return .transitioning }
        return .stopped
    }
}

struct StatusDot: View {
    let tone: StatusTone
    var body: some View {
        Circle().fill(tone.color).frame(width: 8, height: 8).accessibilityHidden(true)
    }
}

/// A dot plus coloured text, for captions in lists.
struct StatusText: View {
    let text: String
    let tone: StatusTone
    var body: some View {
        HStack(spacing: 5) {
            StatusDot(tone: tone)
            Text(text).font(.caption).foregroundStyle(tone.color)
        }
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(tone.label)：\(text)")
    }
}

/// A capsule for the main status of a page.
struct StatusBadge: View {
    let text: String
    let tone: StatusTone
    var body: some View {
        HStack(spacing: 6) {
            StatusDot(tone: tone)
            Text(text).fontWeight(.semibold).foregroundStyle(tone.color)
        }
        .padding(.horizontal, 10).padding(.vertical, 4)
        .background(tone.color.opacity(0.14), in: Capsule())
        .overlay(Capsule().strokeBorder(tone.color.opacity(0.35), lineWidth: 0.5))
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(tone.label)：\(text)")
    }
}
