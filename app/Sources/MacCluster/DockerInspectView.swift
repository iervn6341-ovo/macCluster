import SwiftUI
import AppKit

struct DockerInspectView: View {
    let workload: Workload
    @Environment(\.modalDismiss) private var dismiss
    @State private var output = ""
    @State private var error: String?
    @State private var loading = false
    @State private var generation = 0

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text("容器詳細資訊").font(.title2.bold())
                Spacer()
                if loading { ProgressView().controlSize(.small) }
                Button("重新讀取") { Task { await refresh() } }.disabled(loading)
                Button("複製 JSON") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(output, forType: .string)
                }.disabled(loading || output.isEmpty || error != nil)
                Button("完成") { dismiss() }.keyboardShortcut(.cancelAction)
            }.padding(12).sheetPanel()
            Text(workload.name).font(.headline)
            Text("docker inspect --type container \(workload.containerID ?? "")")
                .font(.system(.caption, design: .monospaced)).textSelection(.enabled)
            if let error { Text(error).foregroundStyle(.red).textSelection(.enabled) }
            NativeLogView(text: loading ? "讀取中…" : output,
                          identity: "inspect:\(workload.containerID ?? ""):\(generation)",
                          scrollAction: 0, startsAtBottom: false)
        }
        .padding(20)
        .frame(width: 850)
        .appScreen()
        .task { await refresh() }
    }

    @MainActor private func refresh() async {
        guard !loading, let id = workload.containerID else { return }
        loading = true
        error = nil
        output = ""
        defer { loading = false }
        do {
            let input = try JSONEncoder().encode(["containerID": id])
            let data = try await AgentClient.command(["docker-inspect"], input: input)
            let object = try JSONSerialization.jsonObject(with: data)
            let formatted = try JSONSerialization.data(withJSONObject: object, options: [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes])
            output = String(decoding: formatted, as: UTF8.self)
            generation += 1
        } catch {
            self.error = "無法讀取容器資訊：\(error.localizedDescription)"
        }
    }
}
