import SwiftUI
import AppKit

/// One published port as the detail page lists it. Docker containers (the ones Docker Desktop shows and the Pods
/// Docker runs for the cluster) and native programs all go through this, so every service reads the same way.
struct PortRow: Identifiable, Equatable, Sendable {
    let hostPort: String
    let reach: String
    /// Port inside the container, with protocol ("80/tcp"); nil for native programs.
    let target: String?
    var id: String { hostPort + "|" + (target ?? "") }

    static let loopback = "僅此 Mac"
    static let allInterfaces = "所有網路介面"

    /// Parses `docker ps` notation: "0.0.0.0:3000->8080/tcp, [::]:3000->8080/tcp, 9090/tcp".
    /// IPv4 and IPv6 forms of the same mapping become one row; a port that is only declared (not published)
    /// is returned in `unpublished`.
    static func parse(_ text: String) -> (rows: [PortRow], unpublished: [String]) {
        var rows: [PortRow] = []
        var reaches: [String: [String]] = [:]
        var unpublished: [String] = []
        for entry in text.split(separator: ",").map({ $0.trimmingCharacters(in: .whitespaces) }) where !entry.isEmpty {
            guard let arrow = entry.range(of: "->") else {
                unpublished.append(entry)
                continue
            }
            let host = String(entry[..<arrow.lowerBound])
            let target = String(entry[arrow.upperBound...])
            guard let colon = host.lastIndex(of: ":") else { continue }
            let address = String(host[..<colon]).trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
            let port = String(host[host.index(after: colon)...])
            let reach: String
            switch address {
            case "127.0.0.1", "::1": reach = loopback
            case "0.0.0.0", "::", "": reach = allInterfaces
            default: reach = address
            }
            let row = PortRow(hostPort: port, reach: reach, target: target)
            if reaches[row.id] == nil { rows.append(row) }
            if !(reaches[row.id] ?? []).contains(reach) { reaches[row.id, default: []].append(reach) }
        }
        rows = rows.map { PortRow(hostPort: $0.hostPort, reach: (reaches[$0.id] ?? [$0.reach]).joined(separator: "、"), target: $0.target) }
        return (rows, unpublished)
    }

    /// The addresses a program listens on, as lsof reports them, with what that means for who can connect.
    /// `*` is lsof's wildcard and is shown as 0.0.0.0; `0.0.0.0` and `::` are every interface (the LAN can
    /// connect); `127.0.0.1` and `::1` only this Mac; any other address is that interface.
    static func scope(of addresses: [String]) -> String {
        let all: Set<String> = ["*", "0.0.0.0", "::"]
        let local: Set<String> = ["127.0.0.1", "::1", "localhost"]
        var seen = Set<String>()
        return addresses.compactMap { raw -> String? in
            let address = raw == "*" ? "0.0.0.0" : raw
            guard seen.insert(address).inserted else { return nil }
            if all.contains(raw) { return address + "（所有網路介面，區網可連）" }
            if local.contains(raw) { return address + "（僅此 Mac）" }
            return address + "（區網可連）"
        }.joined(separator: "、")
    }

    static func rows(for workload: Workload) -> (rows: [PortRow], unpublished: [String]) {
        if let mappings = workload.portMappings ?? workload.ports, !mappings.isEmpty {
            return parse(mappings)
        }
        if workload.executor == "native" {
            let running = workload.phase == "Running"
            func reach(_ port: Int, fallback: String, suffix: String = "") -> String {
                if let found = workload.listening?.first(where: { $0.port == port }) { return scope(of: found.addresses) + suffix }
                return running ? "尚未開始監聽（偵測中）" : fallback
            }
            if let fixed = workload.fixedPorts, !fixed.isEmpty {
                return (fixed.map { PortRow(hostPort: String($0), reach: reach($0, fallback: "程式自行綁定（固定埠）"), target: nil) }, [])
            }
            if let port = workload.port, port > 0 {
                return ([PortRow(hostPort: String(port), reach: reach(port, fallback: "動態分配，以 MACCLUSTER_PORT 提供", suffix: "（動態分配，MACCLUSTER_PORT）"), target: nil)], [])
            }
            return ([], [])
        }
        if let port = workload.port, port > 0 {
            return ([PortRow(hostPort: String(port), reach: loopback, target: nil)], [])
        }
        return ([], [])
    }
}

/// A labelled value: fixed-width label, flexible value. Plain stacks only; a Grid inside the group boxes of
/// the detail page made layout recurse until the window stopped responding.
struct InfoRow<Value: View>: View {
    let label: String
    @ViewBuilder var value: Value
    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 14) {
            Text(label).foregroundStyle(.secondary).frame(width: 96, alignment: .trailing)
            value.frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

/// "連接埠": the same table for local Docker containers, Docker Pods and native programs.
struct PortsCard: View {
    let workload: Workload
    var body: some View {
        let (rows, unpublished) = PortRow.rows(for: workload)
        let showTarget = rows.contains { $0.target != nil }
        CollapsibleCard("連接埠", systemImage: "network", storageKey: "ports") {
            VStack(alignment: .leading, spacing: 8) {
                if rows.isEmpty && unpublished.isEmpty {
                    Text("沒有公開連接埠").foregroundStyle(.secondary)
                }
                if !rows.isEmpty {
                    HStack(spacing: 14) {
                        Text("本機埠").frame(width: 110, alignment: .leading)
                        Text("可連線範圍").frame(maxWidth: .infinity, alignment: .leading)
                        if showTarget { Text("容器內埠").frame(width: 110, alignment: .leading) }
                    }
                    .font(.caption).foregroundStyle(.secondary)
                    Divider()
                    ForEach(rows) { row in
                        HStack(spacing: 14) {
                            Text(row.hostPort).font(.system(.body, design: .monospaced)).fontWeight(.medium)
                                .frame(width: 110, alignment: .leading)
                            Text(row.reach).frame(maxWidth: .infinity, alignment: .leading)
                            if showTarget {
                                Text(row.target ?? "—").font(.system(.body, design: .monospaced)).foregroundStyle(.secondary)
                                    .frame(width: 110, alignment: .leading)
                            }
                        }
                    }
                }
                if !unpublished.isEmpty {
                    Text("僅映像宣告、未發佈到本機：" + unpublished.joined(separator: "、"))
                        .font(.caption).foregroundStyle(.secondary)
                }
                if workload.portsConfigured == true {
                    Label("容器未執行，顯示的是建立時的設定", systemImage: "info.circle")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

/// Compose labels of a container (or of the first container of a project), as labelled rows.
struct ComposeRows: View {
    let project: String
    let service: String?
    let directory: String?
    let files: String?
    var body: some View {
        InfoRow(label: "Compose 專案") { Text(project).textSelection(.enabled) }
        if let service, !service.isEmpty { InfoRow(label: "服務") { Text(service).textSelection(.enabled) } }
        if let directory, !directory.isEmpty {
            InfoRow(label: "工作目錄") { Text(directory).font(.system(.callout, design: .monospaced)).textSelection(.enabled) }
        }
        if let files, !files.isEmpty {
            InfoRow(label: "設定檔") { Text(files).font(.system(.callout, design: .monospaced)).textSelection(.enabled) }
        }
    }
}

/// "容器資訊": image, ID and, for Compose containers, where they came from, as labelled rows
/// instead of separate blocks of text.
struct ContainerInfoCard: View {
    let workload: Workload
    var body: some View {
        CollapsibleCard("容器資訊", systemImage: "shippingbox", storageKey: "containerInfo") {
            VStack(alignment: .leading, spacing: 8) {
                if let image = workload.image, !image.isEmpty {
                    InfoRow(label: "映像") { Text(image).textSelection(.enabled) }
                }
                if let id = workload.containerID, !id.isEmpty {
                    InfoRow(label: "容器 ID") {
                        HStack(spacing: 8) {
                            Text(String(id.prefix(12))).font(.system(.body, design: .monospaced)).textSelection(.enabled)
                            Button {
                                NSPasteboard.general.clearContents()
                                NSPasteboard.general.setString(id, forType: .string)
                            } label: { Image(systemName: "doc.on.doc") }
                                .buttonStyle(.plain).foregroundStyle(.secondary)
                                .help("複製完整容器 ID")
                                .accessibilityLabel("複製容器 ID")
                        }
                    }
                }
                if let project = workload.composeProject, !project.isEmpty {
                    Divider()
                    ComposeRows(project: project, service: workload.composeService,
                                directory: workload.composeWorkingDir, files: workload.composeConfigFiles)
                    Text("顯示容器建立時的 Compose 標籤；原始檔案可能已移動或變更。")
                        .font(.caption).foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}
