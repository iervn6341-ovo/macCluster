import SwiftUI

struct DockerStatsSnapshot: Decodable {
    let running: Bool
    let metrics: [String: String]
}

struct DockerStatsView: View {
    let workload: Workload
    @Environment(\.modalDismiss) private var dismiss
    @State private var snapshot: DockerStatsSnapshot?
    @State private var error: String?
    @State private var loading = false
    @State private var updated: Date?
    @State private var autoRefresh = true

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                Text("資源使用狀況").font(.title2.bold())
                Spacer()
                if loading { ProgressView().controlSize(.small) }
                Button("重新讀取") { Task { await refresh() } }.disabled(loading)
                Button("完成") { dismiss() }.keyboardShortcut(.cancelAction)
            }.padding(12).sheetPanel()
            Text(workload.name).font(.headline)
            Text("docker stats --no-stream \(workload.containerID ?? "")")
                .font(.system(.caption, design: .monospaced)).textSelection(.enabled)
            Toggle("自動更新（每次取樣完成後約 3 秒）", isOn: $autoRefresh)
            if let updated { Text("最後成功更新：\(updated.formatted(date: .omitted, time: .standard))").font(.caption).foregroundStyle(.secondary) }
            if let error { Text(error).foregroundStyle(.red).textSelection(.enabled) }
            if let snapshot, error == nil {
                if snapshot.running {
                    Grid(alignment: .leading, horizontalSpacing: 32, verticalSpacing: 14) {
                        metric("CPU", "CPUPerc", snapshot)
                        metric("記憶體用量／上限", "MemUsage", snapshot)
                        metric("記憶體比例", "MemPerc", snapshot)
                        metric("網路接收／傳送", "NetIO", snapshot)
                        metric("磁碟讀取／寫入", "BlockIO", snapshot)
                        metric("PIDs", "PIDs", snapshot)
                    }.textSelection(.enabled)
                    Text("網路與磁碟 I/O 為累計量，不是每秒速率。")
                        .font(.caption).foregroundStyle(.secondary)
                } else {
                    Text("容器目前未執行，沒有即時資源用量。")
                }
            } else if error == nil {
                Text("正在取得資源用量…").foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
        }
        .padding(24)
        .frame(width: 700)
        .appScreen()
        .task {
            await refresh()
            while !Task.isCancelled {
                do { try await Task.sleep(for: .seconds(3)) } catch { break }
                if autoRefresh { await refresh() }
            }
        }
    }

    private func metric(_ label: String, _ key: String, _ snapshot: DockerStatsSnapshot) -> some View {
        GridRow {
            Text(label).foregroundStyle(.secondary)
            Text(snapshot.metrics[key] ?? "—").monospacedDigit()
        }
    }

    @MainActor private func refresh() async {
        guard !loading, !Task.isCancelled, let id = workload.containerID else { return }
        loading = true
        defer { loading = false }
        do {
            let input = try JSONEncoder().encode(["containerID": id])
            let data = try await AgentClient.command(["docker-stats"], input: input)
            guard !Task.isCancelled else { return }
            snapshot = try JSONDecoder().decode(DockerStatsSnapshot.self, from: data)
            updated = Date()
            error = nil
        } catch {
            self.error = "無法讀取資源用量：\(error.localizedDescription)"
        }
    }
}
