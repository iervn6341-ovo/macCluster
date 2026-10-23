import SwiftUI
import Network

/// One line of the health check page.
struct HealthCheckItem: Decodable, Identifiable, Equatable, Sendable {
    enum Status: String, Decodable, Sendable { case ok, warn, fail, skip }
    let id: String
    let title: String
    let status: Status
    var detail: String?
    var hint: String?
    /// Names a button the page can show: "open-local-network-settings" or "open-docker-desktop".
    var action: String?

    /// Keep endpoint addresses in diagnostics, but show a generic control-plane label on this page.
    func displayed(endpoints: [String]) -> Self {
        let addresses = Set(endpoints.flatMap { endpoint -> [String] in
            guard let url = URL(string: endpoint), let host = url.host else { return [endpoint] }
            return [endpoint, "\(host):\(url.port ?? 443)", host]
        }).filter { !$0.isEmpty }.sorted { $0.count > $1.count }
        func label(_ text: String) -> String {
            addresses.reduce(text) { $0.replacingOccurrences(of: $1, with: "控制平面") }
        }
        return .init(id: id, title: label(title), status: status,
                     detail: detail.map(label), hint: hint.map(label), action: action)
    }
}

/// What the agent reports for GET /v1/netcheck. The agent runs the checks itself because macOS grants the Local
/// Network setting per program: that the app can reach the control plane says nothing about the background agent.
struct NetCheckResult: Decodable, Sendable {
    let checkedAt: String
    let checks: [HealthCheckItem]
}

/// macOS has no API that reports the Local Network setting, so it is inferred: a connection to a private address that
/// fails with "No route to host" while ping (an Apple-signed tool the setting does not apply to) gets through.
enum LocalNetworkProbe {
    enum Outcome: Equatable, Sendable {
        case reachable
        case posix(POSIXErrorCode)
        case timedOut
        case other(String)
    }

    static let settingsURL = URL(string: "x-apple.systempreferences:com.apple.preference.security?Privacy_LocalNetwork")!

    /// A host that is on the local network: a private or link-local IPv4 address, or a .local name.
    static func isLocalNetworkHost(_ host: String) -> Bool {
        if host.lowercased().hasSuffix(".local") { return true }
        let parts = host.split(separator: ".").compactMap { Int($0) }
        guard parts.count == 4, parts.allSatisfy({ (0...255).contains($0) }) else { return false }
        switch (parts[0], parts[1]) {
        case (10, _), (192, 168), (169, 254): return true
        case (172, 16...31): return true
        default: return false
        }
    }

    static func endpointParts(_ endpoint: String) -> (host: String, port: UInt16)? {
        guard let url = URL(string: endpoint), let host = url.host, !host.isEmpty else { return nil }
        return (host, UInt16(url.port ?? 443))
    }

    /// Delivers the first outcome only: a connection can report ready, waiting and a timeout one after another.
    private final class FirstOutcome: @unchecked Sendable {
        private let lock = NSLock()
        private var continuation: CheckedContinuation<Outcome, Never>?
        private let connection: NWConnection
        init(_ continuation: CheckedContinuation<Outcome, Never>, _ connection: NWConnection) {
            self.continuation = continuation
            self.connection = connection
        }
        func finish(_ outcome: Outcome) {
            lock.lock()
            let pending = continuation
            continuation = nil
            lock.unlock()
            guard let pending else { return }
            connection.cancel()
            pending.resume(returning: outcome)
        }
    }

    /// Connects from this process. The first attempt to a local-network address is what makes macOS ask the user.
    static func connect(host: String, port: UInt16, timeout: TimeInterval = 4) async -> Outcome {
        guard let nwPort = NWEndpoint.Port(rawValue: port) else { return .other("埠號無效") }
        return await withCheckedContinuation { continuation in
            let connection = NWConnection(host: NWEndpoint.Host(host), port: nwPort, using: .tcp)
            let queue = DispatchQueue(label: "maccluster.localnetwork.probe")
            let once = FirstOutcome(continuation, connection)
            connection.stateUpdateHandler = { state in
                switch state {
                case .ready: once.finish(.reachable)
                case .failed(let error), .waiting(let error):
                    if case .posix(let code) = error { once.finish(.posix(code)) } else { once.finish(.other("\(error)")) }
                default: break
                }
            }
            connection.start(queue: queue)
            queue.asyncAfter(deadline: .now() + timeout) { once.finish(.timedOut) }
        }
    }

    static func ping(_ host: String) async -> Bool {
        (try? await AgentClient.run(URL(fileURLWithPath: "/sbin/ping"), ["-c", "1", "-t", "2", host], timeout: 5)) != nil
    }

    /// The app's own result as a line of the page.
    static func item(for outcome: Outcome, pingOK: Bool, address: String) -> HealthCheckItem {
        let title = "區域網路權限（App）"
        switch outcome {
        case .reachable:
            return .init(id: "app-local-network", title: title, status: .ok, detail: "App 可以連到 \(address)")
        case .posix(.EHOSTUNREACH) where pingOK:
            return .init(id: "app-local-network", title: title, status: .fail,
                         detail: "App 連 \(address) 失敗（No route to host），但 ping 可以通。",
                         hint: "很可能是 macOS 的「區域網路」權限沒有授予 MacCluster。請到「系統設定 → 隱私權與安全性 → 區域網路」開啟它。",
                         action: "open-local-network-settings")
        case .posix(.EHOSTUNREACH):
            return .init(id: "app-local-network", title: title, status: .fail,
                         detail: "App 連 \(address) 失敗（No route to host），ping 也不通。",
                         hint: "這台 Mac 到不了該位址，請檢查網路連線、路由或防火牆。")
        case .posix(.ECONNREFUSED):
            return .init(id: "app-local-network", title: title, status: .warn,
                         detail: "\(address) 可達，但沒有服務在監聽。",
                         hint: "網路與權限沒有問題；請確認該位址的服務。")
        case .timedOut:
            return .init(id: "app-local-network", title: title, status: .warn, detail: "連 \(address) 逾時。",
                         hint: "封包沒有回應，可能被防火牆丟棄，也可能位址有誤。")
        case .posix(let code):
            return .init(id: "app-local-network", title: title, status: .fail, detail: "連 \(address) 失敗：\(code)")
        case .other(let text):
            return .init(id: "app-local-network", title: title, status: .fail, detail: "連 \(address) 失敗：\(text)")
        }
    }
}

@MainActor final class HealthCheckModel: ObservableObject {
    @Published var items: [HealthCheckItem] = []
    @Published var running = false
    @Published var checkedAt: Date?

    func run(model: AppModel) async {
        guard !running else { return }
        running = true
        defer { running = false }
        var found: [HealthCheckItem] = []

        // 1. The background service answers at all.
        let agentUp = model.status != nil && model.error == nil
        found.append(.init(id: "agent", title: "背景管理程式（agent）", status: agentUp ? .ok : .fail,
                           detail: agentUp ? "運作中" : "未連線",
                           hint: agentUp ? nil : "到「管理程式」分頁啟動或重新啟動。"))

        // 2. The app's own reach of the control plane (this is also what makes macOS ask for the permission).
        let endpoint = model.status?.allClusters.first?.endpoint
        if let endpoint, let target = LocalNetworkProbe.endpointParts(endpoint) {
            let address = "控制平面"
            let outcome = await LocalNetworkProbe.connect(host: target.host, port: target.port)
            let pingOK: Bool
            if case .posix(.EHOSTUNREACH) = outcome { pingOK = await LocalNetworkProbe.ping(target.host) } else { pingOK = false }
            found.append(LocalNetworkProbe.item(for: outcome, pingOK: pingOK, address: address))
        } else {
            found.append(.init(id: "app-local-network", title: "區域網路權限（App）", status: .skip, detail: "尚未加入任何叢集"))
        }

        // 3. What the agent itself reaches (the program that does the work).
        if agentUp {
            do {
                let data = try await AgentClient.command(["netcheck"], timeout: 25)
                found.append(contentsOf: try JSONDecoder().decode(NetCheckResult.self, from: data).checks)
            } catch {
                found.append(.init(id: "agent-netcheck", title: "agent 的網路檢查", status: .warn,
                                   detail: "無法取得：\(error.localizedDescription)",
                                   hint: "管理程式可能是舊版，請重新啟動管理程式以更新。"))
            }
        } else {
            found.append(.init(id: "agent-netcheck", title: "agent 的網路檢查", status: .skip, detail: "背景管理程式沒有運作，略過"))
        }

        // 4. Docker.
        if let problem = model.inventoryError {
            found.append(.init(id: "docker", title: "Docker", status: .fail, detail: problem,
                               hint: "請啟動 Docker Desktop；它啟動後會自動重新整理。", action: "open-docker-desktop"))
        } else {
            found.append(.init(id: "docker", title: "Docker", status: .ok, detail: "可以讀取容器清單"))
        }
        items = found.map { $0.displayed(endpoints: model.clusters.map(\.endpoint)) }
        checkedAt = Date()
    }
}

struct HealthCheckSettingsPage: View {
    @EnvironmentObject var model: AppModel
    @StateObject private var health = HealthCheckModel()

    private func symbol(_ status: HealthCheckItem.Status) -> (String, Color) {
        switch status {
        case .ok: return ("checkmark.circle.fill", .green)
        case .warn: return ("exclamationmark.triangle.fill", .orange)
        case .fail: return ("xmark.octagon.fill", .red)
        case .skip: return ("minus.circle", .secondary)
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Text("健康檢查").font(.title3.bold())
                Spacer()
                if health.running { ProgressView().controlSize(.small) }
                if let at = health.checkedAt {
                    Text("檢查時間：\(at.formatted(date: .omitted, time: .standard))").font(.caption).foregroundStyle(.secondary)
                }
                Button("重新檢查") { Task { await health.run(model: model) } }.disabled(health.running)
            }
            Text("確認這台 Mac 能不能連到叢集。macOS 的「區域網路」權限是各程式分別授予的，所以 App 與背景管理程式分開檢查；結果是從連線行為推測，不是系統明確告知。")
                .font(.system(size: 12)).foregroundStyle(.secondary)
            ForEach(health.items) { item in
                let (name, color) = symbol(item.status)
                HStack(alignment: .top, spacing: 10) {
                    Image(systemName: name).foregroundStyle(color).font(.system(size: 18)).frame(width: 22)
                        .accessibilityHidden(true)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(item.title).font(.system(size: 13, weight: .semibold))
                        if let detail = item.detail { Text(detail).font(.system(size: 12)).foregroundStyle(.secondary).textSelection(.enabled) }
                        if let hint = item.hint { Text(hint).font(.system(size: 12)) }
                        if item.action == "open-local-network-settings" {
                            Button("開啟「區域網路」設定") { NSWorkspace.shared.open(LocalNetworkProbe.settingsURL) }
                        } else if item.action == "open-docker-desktop" {
                            Button("開啟 Docker Desktop") { model.openDockerDesktop() }
                        }
                    }
                    Spacer(minLength: 0)
                }
                .padding(10)
                .background(.quaternary.opacity(0.4), in: RoundedRectangle(cornerRadius: 10))
                .accessibilityElement(children: .combine)
                .accessibilityLabel("\(item.title)，\(item.status.rawValue)，\(item.detail ?? "")")
            }
            if health.items.isEmpty && !health.running { Text("尚未檢查。").foregroundStyle(.secondary) }
        }
        .task { await health.run(model: model) }
    }
}
