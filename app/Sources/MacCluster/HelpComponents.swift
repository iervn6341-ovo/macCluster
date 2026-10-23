import SwiftUI
import AppKit

/// Building blocks for the help pages. They exist so a page reads as cards,
/// tables and code instead of paragraphs.

struct HelpCard<Content: View>: View {
    let title: String
    let systemImage: String
    @ViewBuilder var content: Content
    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Label(title, systemImage: systemImage).font(.headline)
            content
        }
        .padding(14)
        .frame(maxWidth: .infinity, alignment: .leading)
        .sheetPanel()
    }
}

/// One short fact: an icon, a bold lead and an optional one-line detail.
struct FactRow: View {
    let systemImage: String
    var tint: Color = .accentColor
    let title: String
    var detail: String?
    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Image(systemName: systemImage).foregroundStyle(tint).frame(width: 20)
            VStack(alignment: .leading, spacing: 2) {
                Text(title).fontWeight(.medium)
                if let detail { Text(detail).font(.callout).foregroundStyle(.secondary) }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .fixedSize(horizontal: false, vertical: true)
        }
    }
}

struct Callout: View {
    enum Kind {
        case info, warning
        var icon: String { self == .info ? "info.circle.fill" : "exclamationmark.triangle.fill" }
        var color: Color { self == .info ? .blue : .orange }
    }
    let kind: Kind
    var title: String?
    let text: String
    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: kind.icon).foregroundStyle(kind.color)
            VStack(alignment: .leading, spacing: 2) {
                if let title { Text(title).fontWeight(.semibold) }
                Text(text).font(.callout).foregroundStyle(.secondary)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .fixedSize(horizontal: false, vertical: true)
        }
        .padding(12)
        .background(kind.color.opacity(0.12), in: RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(kind.color.opacity(0.35), lineWidth: 0.5))
    }
}

struct HelpTable: View {
    var header: [String] = []
    let rows: [[String]]
    var monospacedFirstColumn = true
    var body: some View {
        Grid(alignment: .topLeading, horizontalSpacing: 16, verticalSpacing: 8) {
            if !header.isEmpty {
                GridRow {
                    ForEach(header.indices, id: \.self) { Text(header[$0]).font(.caption.bold()).foregroundStyle(.secondary) }
                }
                Divider().gridCellColumns(header.count)
            }
            ForEach(rows.indices, id: \.self) { r in
                GridRow {
                    ForEach(rows[r].indices, id: \.self) { c in
                        Text(rows[r][c])
                            .font(c == 0 && monospacedFirstColumn ? .system(.callout, design: .monospaced) : .callout)
                            .foregroundStyle(c == 0 ? .primary : .secondary)
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
            }
        }
    }
}

enum CodeLanguage: String { case yaml, bash }

/// A code listing with a language chip, a copy button and light syntax color.
struct CodeBlock: View {
    var title: String?
    var language: CodeLanguage = .yaml
    let code: String
    var badge: String?
    @State private var copied = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 8) {
                Text(language.rawValue.uppercased()).font(.caption2.bold())
                    .padding(.horizontal, 6).padding(.vertical, 2)
                    .background(.primary.opacity(0.12), in: Capsule())
                if let title { Text(title).font(.caption).foregroundStyle(.secondary).lineLimit(1) }
                if let badge {
                    Text(badge).font(.caption2.bold()).foregroundStyle(.purple)
                        .padding(.horizontal, 6).padding(.vertical, 2)
                        .background(Color.purple.opacity(0.15), in: Capsule())
                }
                Spacer()
                Button(copied ? "已複製" : "複製", systemImage: copied ? "checkmark" : "doc.on.doc") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(code + "\n", forType: .string)
                    copied = true
                    Task { try? await Task.sleep(for: .seconds(1.6)); copied = false }
                }
                .glassSmallButton().font(.caption)
            }
            .padding(.horizontal, 12).padding(.vertical, 8)
            Divider()
            ScrollView(.horizontal, showsIndicators: true) {
                Text(CodeBlock.highlight(code, language: language))
                    .font(.system(size: 12, design: .monospaced))
                    .textSelection(.enabled)
                    .fixedSize()
                    .padding(12)
            }
        }
        .background(Color(nsColor: .textBackgroundColor).opacity(0.55), in: RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(.primary.opacity(0.14), lineWidth: 0.5))
    }

    /// Comments are dimmed and YAML keys are tinted; everything else stays plain.
    static func highlight(_ code: String, language: CodeLanguage) -> AttributedString {
        var out = AttributedString()
        let lines = code.components(separatedBy: "\n")
        for (index, line) in lines.enumerated() {
            let trimmed = line.drop(while: { $0 == " " })
            if trimmed.hasPrefix("#") {
                var a = AttributedString(line); a.foregroundColor = Color.secondary; out += a
            } else if language == .yaml, let split = keySplit(line) {
                var key = AttributedString(String(line[..<split])); key.foregroundColor = Color.blue
                out += key
                out += AttributedString(String(line[split...]))
            } else {
                out += AttributedString(line)
            }
            if index < lines.count - 1 { out += AttributedString("\n") }
        }
        return out
    }

    /// Index just before the ":" that ends a YAML key, ignoring colons inside quotes or values.
    private static func keySplit(_ line: String) -> String.Index? {
        var i = line.startIndex
        var inQuote: Character?
        while i < line.endIndex {
            let ch = line[i]
            if let q = inQuote { if ch == q { inQuote = nil } }
            else if ch == "\"" || ch == "'" { inQuote = ch }
            else if ch == ":" {
                let next = line.index(after: i)
                if next == line.endIndex || line[next] == " " { return i }
            }
            i = line.index(after: i)
        }
        return nil
    }
}

/// A collapsible, copyable manifest. The prototype node name is replaced by the
/// real Node when this Mac has joined a cluster.
struct HelpExample: View {
    @EnvironmentObject var model: AppModel
    let title: String
    let file: String?
    let yaml: String
    var expanded = false
    var badge: String?
    var note: String?
    @State private var open = false

    private var nodeName: String {
        model.clusters.first { !$0.nodeName.isEmpty }?.nodeName ?? FAQExamples.placeholderNode
    }
    var body: some View {
        DisclosureGroup(isExpanded: $open) {
            VStack(alignment: .leading, spacing: 8) {
                if let note { Callout(kind: .info, text: note) }
                CodeBlock(title: file, code: yaml.replacingOccurrences(of: FAQExamples.placeholderNode, with: nodeName), badge: badge)
            }.padding(.top, 8)
        } label: {
            HStack {
                Text(title).font(.headline)
                if let badge { Text(badge).font(.caption2.bold()).foregroundStyle(.purple) }
            }
        }
        .onAppear { open = expanded }
    }
}
