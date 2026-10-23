import SwiftUI
import AppKit

struct ListeningPort: Decodable, Equatable, Sendable {
    let port: Int
    let addresses: [String]
}
struct Workload: Decodable, Identifiable, Sendable {
    let uid: String
    let name: String
    let namespace: String
    let executor: String
    let phase: String
    let message: String?
    let pid: Int?
    let port: Int?
    let restarts: Int?
    let lastExitAt: String?
    let lastExitCode: Int?
    let lastExitReason: String?
    let failureCount: Int?
    let external: Bool?
    let image: String?
    let ports: String?
    let portsConfigured: Bool?
    let portMappings: String?
    let composeProject: String?
    let composeService: String?
    let composeWorkingDir: String?
    let composeConfigFiles: String?
    let containerID: String?
    let environment: String?
    let fixedPorts: [Int]?
    /// Where a native program was found to listen on its declared ports (the agent asks lsof once and remembers).
    let listening: [ListeningPort]?
    var isExternal: Bool { external == true }
    var id: String { uid }
    var hasFailure: Bool { (failureCount ?? 0) > 0 || phase == "Failed" }
}
struct Cluster: Decodable, Identifiable, Sendable {
    let id: String
    let name: String
    let endpoint: String
    let nodeName: String
    let phase: String
    let message: String?
    let managementConfigured: Bool
    let workloads: [Workload]?
}
/// A registry Docker has a login for (names only: the app never sees the secret).
struct DockerLogin: Decodable, Identifiable, Equatable, Sendable {
    let registry: String
    let store: String?
    var id: String { registry }
    /// Docker's own name for Docker Hub is a URL; show it the way people say it.
    var display: String { registry.contains("index.docker.io") ? "Docker Hub" : registry }
}
struct DockerLoginRequest: Encodable, Sendable {
    let registry: String; let username: String; let password: String
}

/// Whether the agent keeps the Mac from going to sleep (mode: when-workloads, always or never; held: right now).
struct KeepAwakeStatus: Decodable, Sendable, Equatable {
    let mode: String
    let held: Bool
}
struct AgentStatus: Decodable, Sendable {
    let events: [KubernetesEvent]?
    let keepAwake: KeepAwakeStatus?
    let nodeName: String?
    let cluster: String?
    let phase: String
    let message: String?
    let networkMode: String?
    let workloads: [Workload]?
    let clusters: [Cluster]?
    let localWorkloads: [Workload]?
    var allClusters: [Cluster] {
        if let clusters { return clusters }
        guard let cluster, !cluster.isEmpty else { return [] }
        return [Cluster(id: "legacy", name: "既有叢集", endpoint: cluster, nodeName: nodeName ?? "", phase: phase, message: message, managementConfigured: false, workloads: workloads)]
    }
}
struct JoinRequest: Encodable, Sendable {
    let command: String; let nodeName: String; let proxyURL: String; let name: String; let managementKubeconfig: String
}
struct JoinResponse: Decodable { let accepted: Bool }
struct CreateRequest: Encodable, Sendable {
    let name: String; let executor: String; let image: String; let command: [String]; let args: [String]
    let env: [String: String]; let workingDir: String; let containerPort: Int; let restartPolicy: String
    var environment: String = ""
}
struct DockerControlRequest: Encodable, Sendable { let containerID: String; let action: String }
struct DeleteRequest: Encodable, Sendable { let uid: String; let clusterID: String; let mode: String }
struct LeaveRequest: Encodable, Sendable { let clusterID: String }
struct ConfigureRequest: Encodable, Sendable { let clusterID: String; let managementKubeconfig: String }
/// A short note under the cluster information. Not HelpComponents.Callout: that one pins its height with
/// fixedSize, and in this page's layout (a plain stack in the detail column) it made the whole detail
/// column, header included, render blank.
struct ClusterNote: View {
    let warning: Bool
    let title: String?
    let text: String
    var body: some View {
        let color: Color = warning ? .orange : .blue
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: warning ? "exclamationmark.triangle.fill" : "info.circle.fill").foregroundStyle(color)
            VStack(alignment: .leading, spacing: 2) {
                if let title { Text(title).fontWeight(.semibold) }
                Text(text).font(.callout).foregroundStyle(.secondary).textSelection(.enabled)
            }
            Spacer(minLength: 0)
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(color.opacity(0.12), in: RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(color.opacity(0.35), lineWidth: 0.5))
    }
}
struct ComposeProject: Identifiable {
    let id: String
    let workloads: [Workload]
    var runningCount: Int { workloads.filter { $0.phase == "Running" }.count }
}
enum Selection: Hashable { case cluster(String); case workload(String, String); case compose(String); case environment(String) }
enum ClientError: LocalizedError {
    case message(String)
    case agentUnavailable
    case timedOut
    var errorDescription: String? {
        switch self {
        case .message(let text): return text
        case .agentUnavailable: return "管理程式未連線。"
        case .timedOut: return "背景命令執行逾時，請稍後重試。"
        }
    }
}
// Connection failures are represented by the service status, not raw socket diagnostics.
// Keep command output intact for diagnostics and CLI users.
enum AgentErrorPresentation {
    static func message(_ detail: String?) -> String? {
        guard let detail, !detail.localizedCaseInsensitiveContains("agent unavailable"), !detail.contains("管理程式未連線") else { return nil }
        return detail
    }
}
// Every subprocess runs off the main actor. Arguments never contain join secrets.
enum AgentClient {
    static var executable: URL { Bundle.main.bundleURL.appendingPathComponent("Contents/Resources/maccluster-agent") }
    static var toolsExecutable: URL { Bundle.main.bundleURL.appendingPathComponent("Contents/Resources/maccluster-tools") }
    static let stateDirectory: URL = {
        let arguments = ProcessInfo.processInfo.arguments
        if let index = arguments.firstIndex(of: "--state-dir"), arguments.indices.contains(index + 1), !arguments[index + 1].isEmpty {
            return URL(fileURLWithPath: NSString(string: arguments[index + 1]).expandingTildeInPath, isDirectory: true).standardizedFileURL
        }
        // A Finder launch must use the same state as this app's installed agent.
        let plist = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/LaunchAgents/com.maccluster.agent.plist")
        if let data = try? Data(contentsOf: plist),
           let config = (try? PropertyListSerialization.propertyList(from: data, format: nil)) as? [String: Any],
           let installed = config["ProgramArguments"] as? [String],
           installed.contains(executable.path),
           let index = installed.firstIndex(of: "--state-dir"),
           installed.indices.contains(index + 1), installed[index + 1].hasPrefix("/") {
            return URL(fileURLWithPath: installed[index + 1], isDirectory: true).standardizedFileURL
        }
        return FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support/MacCluster")
    }()
    static func run(_ executable: URL, _ arguments: [String], input: Data? = nil, sensitive: Bool = false, timeout: TimeInterval? = nil) async throws -> Data {
        try await Task.detached {
            guard FileManager.default.isExecutableFile(atPath: executable.path) else {
                throw ClientError.message("找不到可執行的代理程式。請使用完整的 MacCluster.app 組建。")
            }
            let process = Process()
            process.executableURL = executable
            process.arguments = arguments
            let output = Pipe(), errors = Pipe(), stdin = Pipe()
            process.standardOutput = output
            process.standardError = errors
            process.standardInput = stdin
            try process.run()
            // A command that never returns must not hold the refresh loop (and the busy spinner) forever.
            let deadline = ProcessDeadline()
            let timeoutWork = timeout.map { duration in
                let work = DispatchWorkItem { deadline.expire(process) }
                DispatchQueue.global().asyncAfter(deadline: .now() + duration, execute: work)
                return work
            }
            defer { timeoutWork?.cancel() }
            // Drain stderr concurrently so neither pipe can block the child.
            let errorRead = Task.detached { errors.fileHandleForReading.readDataToEndOfFile() }
            if let input { try stdin.fileHandleForWriting.write(contentsOf: input) }
            try? stdin.fileHandleForWriting.close()
            let result = output.fileHandleForReading.readDataToEndOfFile()
            process.waitUntilExit()
            let errorData = await errorRead.value
            if deadline.expired { throw ClientError.timedOut }
            guard process.terminationStatus == 0 else {
                let detail = String(data: errorData, encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                throw ClientError.message(sensitive ? "加入要求失敗。請檢查指令格式、背景服務及叢集連線，詳細狀態請見主畫面。" : (detail.isEmpty ? "代理程式失敗（\(process.terminationStatus)）。" : detail))
            }
            return result
        }.value
    }
    static func commandArguments(_ arguments: [String], stateDirectory: URL) -> [String] {
        // Service start/stop/restart/status operate on the login job, not an API state directory.
        if arguments.first == "service", !["install", "config"].contains(arguments.dropFirst().first ?? "") { return arguments }
        return arguments + ["--state-dir", stateDirectory.path]
    }
    static func command(_ arguments: [String], input: Data? = nil, sensitive: Bool = false, timeout: TimeInterval? = nil, stateDirectory: URL = AgentClient.stateDirectory, toolsExecutable: URL = AgentClient.toolsExecutable) async throws -> Data {
        if let request = socketRequest(arguments) {
            do {
                return try await AgentSocketClient.request(socketPath: stateDirectory.appendingPathComponent("agent.sock").path,
                    method: request.method, path: request.path, body: input, timeout: timeout ?? 20)
            } catch {
                if sensitive { throw ClientError.message("加入要求失敗。請檢查指令格式、背景服務及叢集連線，詳細狀態請見主畫面。") }
                throw error
            }
        }
        return try await run(toolsExecutable, commandArguments(arguments, stateDirectory: stateDirectory), input: input, sensitive: sensitive, timeout: timeout)
    }
    static func socketRequest(_ arguments: [String]) -> (method: String, path: String)? {
        guard let command = arguments.first else { return nil }
        let reads = ["status", "netcheck", "logs"]
        let writes = ["join", "create", "delete", "leave", "configure", "drain", "settings"]
        guard reads.contains(command) || writes.contains(command) else { return nil }
        var path = "/v1/" + command
        if command == "logs" {
            func value(_ flag: String) -> String {
                guard let index = arguments.firstIndex(of: flag), arguments.indices.contains(index + 1) else { return "" }
                return arguments[index + 1]
            }
            var query = URLComponents()
            query.queryItems = [URLQueryItem(name: "uid", value: value("--uid")), URLQueryItem(name: "clusterID", value: value("--cluster-id"))]
            path += "?" + (query.percentEncodedQuery ?? "")
        }
        return (reads.contains(command) ? "GET" : "POST", path)
    }
    /// Stops every workload and then the agent itself (the agent shuts down after replying).
    static func drain() async throws {
        _ = try await command(["drain"])
    }
    static func stopService() async throws {
        // Missing launchd jobs count as already stopped; other failures remain errors.
        _ = try await run(toolsExecutable, ["service", "stop"], timeout: 25)
    }
    static func installAndStart() async throws {
        let binary = executable
        guard FileManager.default.isExecutableFile(atPath: binary.path) else { throw ClientError.message("應用程式未包含 maccluster-agent，請先完成組建。") }
        let manager = FileManager.default
        let directory = manager.homeDirectoryForCurrentUser.appendingPathComponent("Library/LaunchAgents")
        try manager.createDirectory(at: directory, withIntermediateDirectories: true)
        try manager.createDirectory(at: stateDirectory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        let label = "com.maccluster.agent"
        let plist = directory.appendingPathComponent(label + ".plist")
        // AbandonProcessGroup: the programs and containers the agent started must outlive it (a stop or restart of the agent
        // leaves them running; the next agent takes them over).
        // The CLI owns the restart policy so GUI and CLI installs use the same supervisor.
        let data = try await command(["service", "config"], timeout: 10)
        guard let config = try PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any],
              let arguments = config["ProgramArguments"] as? [String] else {
            throw ClientError.message("無法取得背景服務的啟動設定。")
        }
        try data.write(to: plist, options: .atomic)
        try manager.setAttributes([.posixPermissions: 0o600], ofItemAtPath: plist.path)
        let domain = "gui/\(getuid())"
        // launchd retains the loaded arguments even after the plist is rewritten. An old translocated
        // bundle path must be unloaded and bootstrapped again; kickstart would retry the stale path.
        let loadedConfiguration: Data?
        do {
            loadedConfiguration = try await run(URL(fileURLWithPath: "/bin/launchctl"), ["print", domain + "/" + label], timeout: 3)
        } catch ClientError.timedOut {
            // An unresponsive launchd is not evidence that the job is missing.
            throw ClientError.timedOut
        } catch { loadedConfiguration = nil }
        if let loadedConfiguration {
            if try LaunchAgentConfiguration.matches(loadedConfiguration, arguments: arguments) {
                _ = try await run(URL(fileURLWithPath: "/bin/launchctl"), ["kickstart", domain + "/" + label], timeout: 10)
                return
            }
            try await stopService()
        }
        // The CLI also handles launchd's brief retiring-job window after bootout.
        _ = try await command(["service", "start"], timeout: 25)
    }
}

enum AgentStartupNotice: Equatable {
    case starting, waiting, ready
    var text: String {
        switch self {
        case .starting: return "正在啟動背景服務…"
        case .waiting: return "已送出啟動要求，等待代理程式回應。"
        case .ready: return "背景服務已回應；關閉視窗後仍會運作。"
        }
    }
}

@MainActor final class AppModel: ObservableObject {
    @Published var status: AgentStatus?
    @Published var error: String?
    @Published var actionError: String?
    @Published var startupError: String?
    @Published private(set) var joinNotice: String?
    @Published private(set) var joinError: String?
    private var pendingJoinID: String?
    @Published private(set) var startupNotice: AgentStartupNotice?
    @Published var busy = false
    @Published private(set) var serviceStarting = false
    @Published var settingsPresented = false
    /// The tab the settings pop-up opens on ("general", "service", "docker" or "health").
    @Published var settingsTab = "general"
    /// Asks, once, before the first connection to a local-network address makes macOS show its permission prompt.
    @Published var localNetworkNote = false
    @Published var logs = "選取工作以讀取日誌。"
    @Published var logError: String?
    @Published var selected: Selection?
    @Published var lastUpdated: Date?
    @Published var notice: String?
    @Published var externalContainers: [Workload] = []
    @Published var inventoryError: String?
    @Published var dockerLogins: [DockerLogin] = []
    @Published var dockerLoginMessage: String?
    @Published var dockerLoginFailed = false
    @Published var dockerLoginBusy = false
    @Published var environments: [EnvStatus] = []
    @Published var environmentError: String?
    let terminals = TerminalSessions()
    private static var acknowledgementKey: String { "acknowledgedFailures:" + AgentClient.stateDirectory.path }
    @Published private var acknowledgedFailures = UserDefaults.standard.dictionary(forKey: AppModel.acknowledgementKey) as? [String: String] ?? [:]
    private let refreshCommand: @Sendable ([String]) async throws -> Data
    private let startAgent: @Sendable () async throws -> Void
    private let stopAgent: @Sendable () async throws -> Void
    private let operationCommand: @Sendable (String, Data, Bool) async throws -> Data
    private let startupStatus: @Sendable () async throws -> Data
    private let startupTimeout: Duration
    private let startupPollInterval: Duration
    private let maintainsTerminalLogs: Bool

    init(refreshCommand: @escaping @Sendable ([String]) async throws -> Data = {
        try await AgentClient.command($0, timeout: 20)
    }, startAgent: @escaping @Sendable () async throws -> Void = {
        try await AgentClient.installAndStart()
    }, stopAgent: @escaping @Sendable () async throws -> Void = {
        try await AgentClient.stopService()
    }, operationCommand: @escaping @Sendable (String, Data, Bool) async throws -> Data = {
        try await AgentClient.command([$0], input: $1, sensitive: $2)
    }, startupStatus: @escaping @Sendable () async throws -> Data = {
        try await AgentClient.command(["status"], timeout: 2)
    }, startupTimeout: Duration = .seconds(12), startupPollInterval: Duration = .milliseconds(300), maintainsTerminalLogs: Bool = true) {
        self.startupStatus = startupStatus
        self.startupTimeout = startupTimeout
        self.startupPollInterval = startupPollInterval
        self.operationCommand = operationCommand
        self.maintainsTerminalLogs = maintainsTerminalLogs
        self.refreshCommand = refreshCommand
        self.startAgent = startAgent
        self.stopAgent = stopAgent
    }

    private func failureSignature(_ workload: Workload) -> String? {
        guard !workload.isExternal, workload.hasFailure else { return nil }
        // Recovery and routine status changes must not make the same event unread.
        return "\(workload.failureCount ?? 0)|\(workload.lastExitAt ?? "")|\(workload.lastExitReason ?? "")" + ((workload.lastExitAt ?? "").isEmpty ? "|\(workload.message ?? "")" : "")
    }
    func hasUnreadFailure(_ workload: Workload, clusterID: String) -> Bool {
        guard let signature = failureSignature(workload) else { return false }
        return acknowledgedFailures[clusterID + ":" + workload.uid] != signature
    }
    func acknowledge(_ workload: Workload, clusterID: String) {
        guard let signature = failureSignature(workload) else { return }
        acknowledgedFailures[clusterID + ":" + workload.uid] = signature
        UserDefaults.standard.set(acknowledgedFailures, forKey: Self.acknowledgementKey)
    }
    private var refreshTask: Task<Bool, Never>?
    var clusters: [Cluster] { status?.allClusters ?? [] }
    var locals: [Workload] { (status?.localWorkloads ?? []) + externalContainers }
    var composeProjects: [ComposeProject] {
        let members = locals.filter { $0.isExternal && !($0.composeProject ?? "").isEmpty }
        return Dictionary(grouping: members, by: { $0.composeProject! }).map {
            ComposeProject(id: $0.key, workloads: $0.value.sorted { $0.name.localizedStandardCompare($1.name) == .orderedAscending })
        }.sorted { $0.id.localizedStandardCompare($1.id) == .orderedAscending }
    }
    var ungroupedLocals: [Workload] {
        locals.filter { !$0.isExternal || ($0.composeProject ?? "").isEmpty }
    }
    var selectedCompose: ComposeProject? {
        guard case .compose(let id) = selected else { return nil }
        return composeProjects.first { $0.id == id }
    }
    var selectedEnvironment: EnvStatus? {
        guard case .environment(let name) = selected else { return nil }
        return environments.first { $0.name == name }
    }
    var selectedClusterID: String {
        switch selected { case .cluster(let id), .workload(let id, _): return id; default: return "" }
    }
    var selectedCluster: Cluster? { clusters.first { $0.id == selectedClusterID } }
    var selectedWorkload: Workload? {
        guard case .workload(let id, let uid) = selected else { return nil }
        return (id.isEmpty ? locals : clusters.first { $0.id == id }?.workloads ?? []).first { $0.uid == uid }
    }
    var failures: Int { (locals + clusters.flatMap { $0.workloads ?? [] }).filter(\.hasFailure).count }
    @discardableResult func refresh() async -> Bool {
        if let refreshTask { return await refreshTask.value }
        let task = Task { await refreshSnapshot() }
        refreshTask = task
        let connected = await task.value
        refreshTask = nil
        return connected
    }
    private func refreshSnapshot() async -> Bool {
        // Docker discovery does not depend on the agent being ready. Each request updates its own state
        // as soon as it completes; a slow status or environment request cannot hold the inventory back.
        async let statusReady = refreshStatus()
        async let inventory: Void = refreshInventory()
        async let environments: Void = refreshEnvironments()
        let (connected, _, _) = await (statusReady, inventory, environments)
        applyDevSelection()
        return connected
    }
    private func refreshStatus(using probe: (@Sendable () async throws -> Data)? = nil) async -> Bool {
        do {
            let data: Data
            if let probe { data = try await probe() } else { data = try await refreshCommand(["status"]) }
            status = try JSONDecoder().decode(AgentStatus.self, from: data)
            error = nil; startupError = nil; lastUpdated = Date()
            updateJoinProgress()
            if !UserDefaults.standard.bool(forKey: "localNetworkExplained"),
               let endpoint = status?.allClusters.first?.endpoint,
               let target = LocalNetworkProbe.endpointParts(endpoint), LocalNetworkProbe.isLocalNetworkHost(target.host) {
                localNetworkNote = true
            }
            if !serviceStarting && (startupNotice == .starting || startupNotice == .waiting) { startupNotice = .ready }
            return true
        } catch { self.error = error.localizedDescription; return false }
    }
    private func refreshInventory() async {
        do {
            externalContainers = try JSONDecoder().decode([Workload]?.self, from: await refreshCommand(["inventory"])) ?? []
            inventoryError = nil
        } catch { inventoryError = "Docker 容器清單無法更新，本機容器資料可能已過期：" + error.localizedDescription }
    }
    private func refreshEnvironments() async {
        do {
            // An older agent prints "null" when there is nothing to list; that is an empty list, not a failure.
            environments = try JSONDecoder().decode([EnvStatus]?.self, from: await refreshCommand(["env", "list", "--json"])) ?? []
            environmentError = nil
            sweepTerminalLogs()
        } catch { environmentError = "執行環境清單無法更新：" + error.localizedDescription }
    }
    private var lastTerminalSweep = Date.distantPast
    /// Terminal logs of environments that are gone, and logs of closed terminals older than a week, are removed.
    /// Only done after a successful environment list, so a failed list can never delete a live environment's log.
    private func sweepTerminalLogs() {
        guard maintainsTerminalLogs, Date().timeIntervalSince(lastTerminalSweep) > 3600 else { return }
        lastTerminalSweep = Date()
        let registered = Set(environments.map(\.name)), open = terminals.running
        Task.detached { TerminalLog.sweep(AgentClient.stateDirectory, registered: registered, open: open) }
    }
    /// Development aid, inert unless MACCLUSTER_DEV_SNAPSHOT_DIR is set: MACCLUSTER_DEV_SELECT=cluster or a
    /// service name selects that page once, so a page can be rendered and dumped without clicking.
    private var devSelectionDone = false
    private func applyDevSelection() {
        guard !devSelectionDone, selected == nil,
              ProcessInfo.processInfo.environment["MACCLUSTER_DEV_SNAPSHOT_DIR"] != nil,
              let want = ProcessInfo.processInfo.environment["MACCLUSTER_DEV_SELECT"] else { return }
        if want == "cluster", let cluster = clusters.first {
            selected = .cluster(cluster.id); devSelectionDone = true
        } else if want.hasPrefix("env:"), environments.contains(where: { $0.name == String(want.dropFirst(4)) }) {
            // MACCLUSTER_DEV_SELECT=env:NAME opens that environment; MACCLUSTER_DEV_TERMINAL_INPUT is typed into its terminal.
            let name = String(want.dropFirst(4))
            selected = .environment(name); devSelectionDone = true
            if let input = ProcessInfo.processInfo.environment["MACCLUSTER_DEV_TERMINAL_INPUT"] {
                Task { await terminals.open(name); try? await Task.sleep(for: .seconds(2)); terminals.view(for: name)?.send(txt: input + "\n") }
            }
        } else if let workload = (externalContainers + locals).first(where: { $0.name == want }) {
            selected = .workload("", workload.uid); devSelectionDone = true
        }
    }
    func removeEnvironment(_ name: String) async {
        busy = true; actionError = nil
        defer { busy = false }
        do {
            terminals.close(name)
            _ = try await AgentClient.command(["env", "remove", "--name", name])
            TerminalLog.delete(AgentClient.stateDirectory, name)
            if selected == .environment(name) { selected = nil }
            notice = "已移除執行環境登記：\(name)（未刪除其目錄）。"
            await refresh()
        } catch { actionError = error.localizedDescription }
    }
    func refreshDockerLogins() async {
        if let data = try? await AgentClient.command(["docker-logins"]), let list = try? JSONDecoder().decode([DockerLogin].self, from: data) {
            dockerLogins = list
        }
    }
    /// The password travels to the agent on standard input and on to `docker login` the same way; it is not kept.
    func dockerLogin(registry: String, username: String, password: String) async -> Bool {
        dockerLoginBusy = true; dockerLoginMessage = nil
        defer { dockerLoginBusy = false }
        do {
            let input = try JSONEncoder().encode(DockerLoginRequest(registry: registry.trimmingCharacters(in: .whitespaces), username: username.trimmingCharacters(in: .whitespaces), password: password))
            _ = try await AgentClient.command(["docker-login"], input: input)
            dockerLoginMessage = "已登入 \(registry.isEmpty ? "Docker Hub" : registry)。"; dockerLoginFailed = false
            await refreshDockerLogins()
            return true
        } catch {
            dockerLoginMessage = error.localizedDescription; dockerLoginFailed = true
            return false
        }
    }
    func dockerLogout(_ login: DockerLogin) async {
        dockerLoginBusy = true; dockerLoginMessage = nil
        defer { dockerLoginBusy = false }
        do {
            let registry = login.registry.contains("index.docker.io") ? "" : login.registry
            _ = try await AgentClient.command(["docker-logout"], input: JSONEncoder().encode(["registry": registry]))
            dockerLoginMessage = "已登出 \(login.display)。"; dockerLoginFailed = false
            await refreshDockerLogins()
        } catch { dockerLoginMessage = error.localizedDescription; dockerLoginFailed = true }
    }
    /// `mode` is when-workloads, always or never; the agent saves it and applies it at once.
    func setKeepAwake(_ mode: String) async {
        actionError = nil
        do {
            _ = try await AgentClient.command(["settings"], input: JSONEncoder().encode(["keepAwake": mode]))
            await refresh()
        } catch { actionError = error.localizedDescription }
    }
    func perform<T: Encodable & Sendable>(_ verb: String, _ request: T, sensitive: Bool = false) async throws {
        busy = true; actionError = nil
        defer { busy = false }
        do {
            let response = try await operationCommand(verb, JSONEncoder().encode(request), sensitive)
            switch verb {
            case "leave": notice = "已離開叢集。"
            case "join":
                let accepted = try JSONDecoder().decode(AcceptedJoin.self, from: response)
                pendingJoinID = accepted.id
                joinError = nil; notice = nil
                joinNotice = "已送出加入叢集要求，正在確認連線。"
                selected = .cluster(accepted.id)
            case "create": notice = "已建立服務，啟動狀態將顯示於清單。"
            case "delete": notice = "已完成刪除要求。"
            case "configure": notice = "已儲存叢集管理設定。"
            default: notice = "操作已完成。"
            }
            await refresh()
        } catch { actionError = error.localizedDescription; throw error }
    }
    private func updateJoinProgress() {
        guard let id = pendingJoinID, let cluster = clusters.first(where: { $0.id == id }) else { return }
        switch cluster.phase {
        case "Ready":
            joinError = nil
            joinNotice = "已加入叢集：\(cluster.name)。"
            pendingJoinID = nil
        case "Error", "Removed":
            joinNotice = nil
            joinError = "加入叢集失敗：" + JoinValidation.explanation(cluster.message ?? "請查看叢集狀態。")
            if cluster.phase == "Removed" { pendingJoinID = nil }
        default:
            joinError = nil
            joinNotice = "正在加入叢集：" + (cluster.message ?? "正在確認連線。")
        }
    }
    private var didInitialize = false
    func initialize() async {
        guard !didInitialize else { return }
        didInitialize = true
        if !(await refresh()) { await startService() }
    }
    func restartService() async {
        guard !serviceStarting else { return }
        busy = true; serviceStarting = true; actionError = nil; startupError = nil
        notice = nil; startupNotice = .starting
        defer { busy = false; serviceStarting = false }
        do {
            try await stopAgent()
            try await startAgent()
            await confirmAgentStartup()
        } catch { startupNotice = nil; startupError = "背景服務未能重新啟動：" + error.localizedDescription }
    }
    private func confirmAgentStartup() async {
        let deadline = ContinuousClock.now.advanced(by: startupTimeout)
        var successes = 0
        while ContinuousClock.now < deadline && !Task.isCancelled {
            successes = await refreshStatus(using: startupStatus) ? successes + 1 : 0
            if successes >= 2 {
                startupNotice = .ready
                Task { await self.refresh() }
                return
            }
            do { try await Task.sleep(for: startupPollInterval) } catch { break }
        }
        startupNotice = .waiting
        startupError = "啟動要求已送出，但管理程式尚未穩定回應。" + (error.map { "\n" + $0 } ?? "請重新整理確認狀態。")
    }
    func controlDocker(_ workload: Workload, action: String) async {
        guard workload.isExternal, let id = workload.containerID else { return }
        try? await perform("docker-control", DockerControlRequest(containerID: id, action: action))
    }
    /// Connects once from the app, which is what makes macOS show its Local Network prompt; if the answer was "don't
    /// allow" (or an earlier one still stands), the health check page opens with the reason.
    func triggerLocalNetworkPrompt() async {
        guard let endpoint = status?.allClusters.first?.endpoint, let target = LocalNetworkProbe.endpointParts(endpoint) else { return }
        let outcome = await LocalNetworkProbe.connect(host: target.host, port: target.port, timeout: 30)
        if case .posix(.EHOSTUNREACH) = outcome {
            settingsTab = "health"
            settingsPresented = true
        }
    }
    func openDockerDesktop() {
        let url = NSWorkspace.shared.urlForApplication(withBundleIdentifier: "com.docker.docker") ?? URL(fileURLWithPath: "/Applications/Docker.app")
        if !NSWorkspace.shared.open(url) { actionError = "無法開啟 Docker Desktop，請確認已安裝。" }
    }
    func startService() async {
        guard !serviceStarting else { return }
        busy = true; serviceStarting = true
        defer { busy = false; serviceStarting = false }
        actionError = nil; startupError = nil; notice = nil; startupNotice = .starting
        do {
            try await startAgent()
            await confirmAgentStartup()
        } catch { startupNotice = nil; startupError = "背景服務未能啟動：" + error.localizedDescription }
    }
    /// `stopWorkloads == false` (the default choice): only the agent stops; programs and containers keep running and
    /// the next agent takes them over. `true`: everything it runs is stopped first.
    func stopService(stopWorkloads: Bool = false) async {
        busy = true; defer { busy = false }
        startupNotice = nil
        do {
            if stopWorkloads {
                try await AgentClient.drain()
                // The agent replies, then stops its workloads and exits; give it time before the launchd stop.
                for _ in 0..<40 {
                    if (try? await AgentClient.command(["status"])) == nil { break }
                    try? await Task.sleep(for: .seconds(1.5))
                }
            }
            try await stopAgent()
            error = "背景服務已停止；下列資料為上次回應。"
            notice = stopWorkloads
                ? "管理程式與它管理的所有工作都已停止。LaunchAgent 設定仍保留，下次登入會再次啟動。"
                : "只停止了管理程式：容器與原生程序仍在執行，但在管理程式回來之前沒有人重啟、探測或回報它們。LaunchAgent 設定仍保留，下次登入會再次啟動並接手。"
        } catch { actionError = error.localizedDescription }
    }
    func loadLogs() async {
        guard case .workload(let clusterID, let uid) = selected else { logs = "選取工作以讀取日誌。"; logError = nil; return }
        let externalContainerID = selectedWorkload?.isExternal == true ? selectedWorkload?.containerID : nil
        let selection = selected
        logs = "讀取中…"; logError = nil
        do {
            let data: Data
            if let id = externalContainerID {
                let input = try JSONEncoder().encode(["containerID": id])
                data = try await AgentClient.command(["docker-logs"], input: input)
            } else {
                data = try await AgentClient.command(["logs", "--uid", uid, "--cluster-id", clusterID])
            }
            guard selected == selection else { return }
            logs = String(data: data, encoding: .utf8) ?? "日誌不是 UTF-8 文字。"
            if logs.isEmpty { logs = "目前沒有日誌。" }
        } catch { if selected == selection { logError = error.localizedDescription; logs = "" } }
    }
}

/// Launchbit's settings layout: heading 16 pt semibold, rows 16 pt apart, notes 12 pt secondary.
private func settingsHeading(_ text: String) -> some View {
    Text(text).font(.system(size: 16, weight: .semibold))
}

/// Docker 登入：只是替使用者執行 `docker login`。憑證由 Docker 自己的認證儲存區（macOS 鑰匙圈）保存，
/// macCluster 不保存密碼；之後 Docker 執行器的 Pod 拉取私有映像時就用這份登入。
private struct DockerLoginSettingsPage: View {
    @EnvironmentObject var model: AppModel
    @State private var registry = ""
    @State private var username = ""
    @State private var password = ""
    private var serviceRunning: Bool { model.status != nil && model.error == nil }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            settingsHeading("Docker 登入")
            Text("登入私有映像庫後，使用 Docker 執行器的 Pod 就能拉取其中的映像。這只是替你執行 docker login：密碼由 Docker 自己的認證儲存區（macOS 鑰匙圈）保存，macCluster 不保存，也不會寫進任何檔案。")
                .font(.callout)
            GroupBox("新增登入") {
                VStack(alignment: .leading, spacing: 8) {
                    TextField("映像庫位址（留空代表 Docker Hub）", text: $registry).textFieldStyle(.roundedBorder).autocorrectionDisabled()
                    TextField("使用者名稱", text: $username).textFieldStyle(.roundedBorder).autocorrectionDisabled()
                    SecureField("密碼或存取權杖", text: $password).textFieldStyle(.roundedBorder)
                    HStack {
                        Button("登入") {
                            let r = registry, u = username, p = password
                            Task { if await model.dockerLogin(registry: r, username: u, password: p) { password = "" } }
                        }
                        .disabled(!serviceRunning || model.dockerLoginBusy || username.trimmingCharacters(in: .whitespaces).isEmpty || password.isEmpty)
                        if model.dockerLoginBusy { ProgressView().controlSize(.small) }
                    }
                    if let message = model.dockerLoginMessage {
                        Label(message, systemImage: model.dockerLoginFailed ? "exclamationmark.triangle.fill" : "checkmark.circle.fill")
                            .foregroundStyle(model.dockerLoginFailed ? .red : .green).font(.callout).textSelection(.enabled)
                    }
                }.frame(maxWidth: .infinity, alignment: .leading)
            }
            GroupBox("目前的登入") {
                VStack(alignment: .leading, spacing: 6) {
                    if model.dockerLogins.isEmpty { Text("沒有已登入的映像庫。").foregroundStyle(.secondary) }
                    ForEach(model.dockerLogins) { login in
                        HStack {
                            Text(login.display).textSelection(.enabled)
                            if let store = login.store, !store.isEmpty { Text("（\(store)）").font(.caption).foregroundStyle(.secondary) }
                            Spacer()
                            Button("登出", role: .destructive) { Task { await model.dockerLogout(login) } }.disabled(model.dockerLoginBusy)
                        }
                    }
                }.frame(maxWidth: .infinity, alignment: .leading)
            }
            Text("叢集的 imagePullSecrets 目前不支援，請改用這裡的登入。叢集內建的 HTTP 映像庫需要先在 Docker Desktop 的設定把它加入 insecure registries。")
                .font(.system(size: 12)).foregroundStyle(.secondary)
        }
        .task { await model.refreshDockerLogins() }
    }
}

private struct GeneralSettingsPage: View {
    @AppStorage("logFontSize") private var fontSize = 12.0
    @AppStorage("logLineSpacing") private var lineSpacing = 0.0
    @AppStorage("logCharacterSpacing") private var characterSpacing = 0.0

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            settingsHeading("日誌顯示")
            Stepper(value: $fontSize, in: 10...32, step: 1) {
                Text("字體大小：\(fontSize, specifier: "%.0f") pt")
            }
            Stepper(value: $characterSpacing, in: 0...5, step: 0.5) {
                Text("字元間距：\(characterSpacing, specifier: "%.1f") pt")
            }
            Stepper(value: $lineSpacing, in: 0...16, step: 1) {
                Text("額外行距：\(lineSpacing, specifier: "%.0f") pt")
            }
            Text("即時套用至服務日誌，並自動保存設定。").font(.system(size: 12)).foregroundStyle(.secondary)
            ScrollView(.vertical) {
                Text(AttributedString(ANSILogText.render(
                    "INFO 服務已啟動，較長的日誌會依視窗寬度自動換行。\n\u{1b}[33mWARNING 範例提醒\u{1b}[0m\n\u{1b}[1;32mSUCCESS 操作完成\u{1b}[0m",
                    fontSize: fontSize, lineSpacing: lineSpacing, characterSpacing: characterSpacing)))
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(8)
            }
            .frame(height: 110)
            .background(Color(nsColor: .textBackgroundColor), in: RoundedRectangle(cornerRadius: 8))
            Button("還原日誌顯示預設值") {
                fontSize = 12; characterSpacing = 0; lineSpacing = 0
            }
        }
    }
}

private struct ServiceSettingsPage: View {
    @EnvironmentObject var model: AppModel
    @Binding var action: String?
    /// The service counts as running only while the agent answers status requests.
    private var serviceRunning: Bool { model.status != nil && model.error == nil }

    private var serviceStatusRow: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 8) {
                Text("目前狀態").foregroundStyle(.secondary)
                if model.serviceStarting {
                    ProgressView().controlSize(.small)
                    Text("正在啟動管理程式…").fontWeight(.semibold)
                        .accessibilityLabel("管理程式正在啟動")
                } else {
                    Label(serviceRunning ? "運行中" : "未運行", systemImage: serviceRunning ? "checkmark.circle.fill" : "xmark.circle.fill")
                        .fontWeight(.semibold)
                        .foregroundStyle(serviceRunning ? .green : .red)
                        .accessibilityLabel("管理程式" + (serviceRunning ? "運行中" : "未運行"))
                }
            }
            if model.serviceStarting {
                Text("正在安裝啟動設定並等待管理程式穩定回應。")
                    .font(.caption).foregroundStyle(.secondary)
            } else if serviceRunning, let updated = model.lastUpdated {
                Text("最後確認：\(updated.formatted(date: .omitted, time: .standard))").font(.caption).foregroundStyle(.secondary)
            }
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            settingsHeading("管理程式")
            Text("App 開啟時會連線或啟動管理程式。安裝後會在登入 macOS 時啟動；關閉視窗不會停止工作。")
            serviceStatusRow
            HStack {
                Button(model.serviceStarting ? "正在啟動…" : "啟動／修復自動啟動") { Task { await model.startService() } }
                Button("重新啟動…") { action = "restart" }
                Button("停止…", role: .destructive) { action = "stop" }
            }.disabled(model.busy)
            if let error = AgentErrorPresentation.message(model.startupError) { Text(error).foregroundStyle(.red).textSelection(.enabled) }
            if let error = AgentErrorPresentation.message(model.actionError ?? model.joinError) { Text(error).foregroundStyle(.red) }
            Text("停止只適用於此 App 安裝的 LaunchAgent；手動從終端啟動的 Agent 請於原終端管理。").font(.system(size: 12)).foregroundStyle(.secondary)
            Divider()
            keepAwakeSection
        }
    }

    /// Mac 睡眠：睡眠時所有工作暫停，節點約 40 秒後變 NotReady（docs/keep-awake-design.md）。
    private var keepAwakeSection: some View {
        let mode = model.status?.keepAwake?.mode ?? "when-workloads"
        return VStack(alignment: .leading, spacing: 8) {
            settingsHeading("睡眠")
            Picker("防止 Mac 閒置睡眠", selection: Binding(get: { mode }, set: { new in Task { await model.setKeepAwake(new) } })) {
                Text("有工作執行時（建議）").tag("when-workloads")
                Text("管理程式執行期間一律").tag("always")
                Text("不處理（依系統設定）").tag("never")
            }
            .pickerStyle(.menu)
            .disabled(!serviceRunning || model.busy)
            if let held = model.status?.keepAwake?.held {
                Label(held ? "目前正在防止睡眠" : "目前沒有阻擋睡眠", systemImage: held ? "moon.zzz.fill" : "moon")
                    .font(.callout).foregroundStyle(held ? .green : .secondary)
            }
            Text("睡眠時 Mac 上所有容器與程式都會暫停，節點約 40 秒後在叢集顯示 NotReady。這個選項只在接上電源（AC）時阻擋閒置睡眠；使用電池時依系統設定。它不會阻止你手動選擇睡眠或闔上螢幕。")
                .font(.system(size: 12)).foregroundStyle(.secondary)
        }
    }
}

/// The tabs of the settings pop-up (Launchbit: an NSTabView whose tabs each scroll).
struct AdvancedSettings: View {
    @EnvironmentObject var model: AppModel
    @State private var action: String?
    private var tab: Binding<String> { $model.settingsTab }

    var body: some View {
        AnimatedTabView(pages: [
            .init(id: "general", title: "一般", content: AnyView(GeneralSettingsPage())),
            .init(id: "service", title: "管理程式", content: AnyView(ServiceSettingsPage(action: $action))),
            .init(id: "docker", title: "Docker 登入", content: AnyView(DockerLoginSettingsPage())),
            .init(id: "health", title: "健康檢查", content: AnyView(HealthCheckSettingsPage()))
        ], selection: tab, barMaxWidth: 400)
        .alert(action == "restart" ? "重新啟動管理程式？" : "停止管理程式？", isPresented: Binding(get: { action != nil }, set: { if !$0 { action = nil } })) {
            Button("取消", role: .cancel) { action = nil }
            if action == "restart" {
                Button("重新啟動") { action = nil; Task { await model.restartService() } }
            } else {
                Button("只停止管理程式") { action = nil; Task { await model.stopService() } }
                Button("停止管理程式與所有工作", role: .destructive) { action = nil; Task { await model.stopService(stopWorkloads: true) } }
            }
        } message: {
            Text(action == "restart"
                 ? "受管理的容器與原生程序會繼續執行，新的管理程式啟動後接手它們；節點回報只中斷幾秒。從舊版升級的那一次，舊版仍會停止工作。"
                 : "「只停止管理程式」\n容器與原生程序繼續執行，但在管理程式回來之前沒有人重啟、探測或回報它們，節點約 40 秒後會顯示 NotReady。\n\n「停止管理程式與所有工作」\n一併停止它管理的所有容器與程序。\n\n解除安裝 App 之前，請先離開叢集或停止所有工作，否則它們會繼續執行而沒人管理。")
        }
    }
}

/// Launchbit's panel layout: the tab view inset 22 pt at the sides, 24 pt at the top and 72 pt at the bottom,
/// and a 120 x 30 "完成" button 25 pt from the right and 24 pt from the bottom.
struct TabbedPanel<Tabs: View>: View {
    let done: () -> Void
    @ViewBuilder let tabs: Tabs
    var body: some View {
        ZStack(alignment: .bottomTrailing) {
            tabs
                .padding(.horizontal, 22).padding(.top, 24).padding(.bottom, 72)
            Button("完成", action: done)
                .keyboardShortcut(.cancelAction)
                .glassButtons()
                .frame(width: 120, height: 30)
                .padding(.trailing, 25).padding(.bottom, 24)
        }
    }
}

@MainActor final class AppDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) { DevSnapshot.installIfRequested() }
    var reopenMainWindow: (() -> Void)?

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
        false
    }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        // Settings may still be visible after the dashboard has been closed.
        // Always ask SwiftUI to show the unique main window.
        reopenMainWindow?()
        sender.activate(ignoringOtherApps: true)
        return false
    }
}

private struct MainWindowContent: View {
    @Environment(\.openWindow) private var openWindow
    let appDelegate: AppDelegate

    var body: some View {
        Dashboard()
            .frame(minWidth: 960, minHeight: 680)
            .onAppear {
                appDelegate.reopenMainWindow = { openWindow(id: "main") }
            }
    }
}

@main struct MacClusterApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @StateObject private var model = AppModel()
    var body: some Scene {
        Window("MacCluster", id: "main") {
            MainWindowContent(appDelegate: appDelegate).environmentObject(model)
        }
        .commands {
            CommandGroup(replacing: .appSettings) {
                Button("設定…") {
                    appDelegate.reopenMainWindow?()
                    model.settingsPresented = true
                }.keyboardShortcut(",", modifiers: .command)
            }
        }
    }
}
enum Editor: String, Identifiable { case join, native, docker, configure; var id: String { rawValue } }
struct PendingAction: Identifiable {
    let id = UUID()
    let title: String
    let message: String
    let request: DeleteRequest?
    let leave: String?
    var environment: String? = nil
}
struct Dashboard: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @State private var editor: Editor?
    @State private var columnVisibility: NavigationSplitViewVisibility = .all
    @State private var configureClusterID = ""
    @State private var logScrollAction = 0
    @State private var showHelp = false
    @State private var showRuntimeLog = false
    @State private var expandedGroups: Set<Selection> = []
    @State private var inspectWorkload: Workload?
    @State private var statsWorkload: Workload?
    @State private var pending: PendingAction?
    @State private var environmentEditor: EnvironmentEditor?
    var body: some View {
        SidebarSplitView(visibility: $columnVisibility) {
            ScrollView {
              VStack(alignment: .leading, spacing: 6) {
                Group {
                    Text("Kubernetes 叢集").font(.caption.bold()).foregroundStyle(.secondary).padding(.top, 8)
                    if model.clusters.isEmpty { Text("尚未加入叢集").foregroundStyle(.secondary) }
                    ForEach(model.clusters) { cluster in
                        DisclosureGroup(isExpanded: expansion(.cluster(cluster.id))) {
                            ForEach(cluster.workloads ?? []) { workload in
                                workloadRow(workload, clusterID: cluster.id)
                            }
                            if (cluster.workloads ?? []).isEmpty { Text("此 Mac 無指派工作").font(.caption).foregroundStyle(.secondary) }
                        } label: {
                            VStack(alignment: .leading) {
                                Label {
                                    Text(cluster.name.isEmpty ? cluster.nodeName : cluster.name)
                                } icon: { KubernetesIcon() }
                                StatusText(text: clusterConnection(cluster), tone: clusterTone(cluster))
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .contentShape(Rectangle())
                            .onTapGesture { model.selected = .cluster(cluster.id) }
                            .simultaneousGesture(TapGesture(count: 2).onEnded {
                                model.selected = .cluster(cluster.id)
                                toggleExpansion(.cluster(cluster.id))
                            })
                            .padding(5)
                            .background { if model.selected == .cluster(cluster.id) { SelectionHighlight() } }
                            .sidebarHover()
                        }
                        .disclosureGroupStyle(SidebarDisclosureStyle())
                        .tag(Selection.cluster(cluster.id))
                        .contextMenu {
                            Button("設定管理 kubeconfig…") { configureClusterID = cluster.id; editor = .configure }
                            Button("離開此叢集…", role: .destructive) { confirmLeave(cluster) }
                        }
                    }
                }
                Group {
                    Text("本機服務").font(.caption.bold()).foregroundStyle(.secondary).padding(.top, 14)
                    if model.locals.isEmpty { Text("尚無本機服務").foregroundStyle(.secondary) }
                    ForEach(model.ungroupedLocals) { workloadRow($0, clusterID: "") }
                    ForEach(model.composeProjects) { project in
                        DisclosureGroup(isExpanded: expansion(.compose(project.id))) {
                            ForEach(project.workloads) { workloadRow($0, clusterID: "") }
                        } label: {
                            VStack(alignment: .leading, spacing: 3) {
                                Label(project.id, systemImage: "square.stack.3d.up")
                                StatusText(text: "Compose · \(project.workloads.count) 個容器 · \(project.runningCount) 個執行中",
                                           tone: composeTone(project))
                            }
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .contentShape(Rectangle())
                            .onTapGesture { model.selected = .compose(project.id) }
                            .simultaneousGesture(TapGesture(count: 2).onEnded {
                                model.selected = .compose(project.id)
                                toggleExpansion(.compose(project.id))
                            })
                            .padding(5)
                            .background { if model.selected == .compose(project.id) { SelectionHighlight() } }
                            .sidebarHover()
                        }
                        .disclosureGroupStyle(SidebarDisclosureStyle())
                        .tag(Selection.compose(project.id))
                    }
                }
              }
              // A vertical ScrollView centers content that is narrower than itself; without this the list shrinks to
              // its widest row and floats in the middle of the sidebar (seen on macOS 27 when every row is short).
              .frame(maxWidth: .infinity, alignment: .leading)
              .padding(8)
              .animation(reduceMotion ? nil : .easeInOut(duration: 0.25), value: expandedGroups)
            }
            .labelStyle(SidebarIconLabelStyle())
            .scrollContentBackground(.hidden)
            .padding(6)
            .sidebarCanvas()
            .navigationTitle("MacCluster")
            .navigationSplitViewColumnWidth(min: 240, ideal: 290)
        } detail: {
            VStack(alignment: .leading, spacing: 14) {
                statusHeader.padding(12).contentCard()
                if let workload = model.selectedWorkload {
                    workloadDetail(workload)
                } else if let project = model.selectedCompose {
                    composeDetail(project)
                    Spacer()
                } else if let environment = model.selectedEnvironment {
                    EnvironmentDetailView(terminals: model.terminals, environment: environment,
                                          edit: { environmentEditor = EnvironmentEditor(editing: environment) },
                                          remove: { confirmRemoveEnvironment(environment) })
                } else if let cluster = model.selectedCluster {
                    clusterDetail(cluster)
                    Spacer()
                } else {
                    ContentUnavailableView("選取叢集或服務", systemImage: "sidebar.left", description: Text("左側 + 可加入叢集或新增本機 Docker／原生服務。"))
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .padding(22)
            .background { AppCanvas() }
            .toolbarBackground(.hidden, for: .windowToolbar)
            .groupBoxStyle(CardGroupBoxStyle())
            .modifier(SidebarDiagnosticDetail())
        }
        .background { if SidebarDiagnostics.enabled { SidebarCadenceProbe() } }
        .toolbar {
          if !SidebarDiagnostics.hideToolbar {
            // On the window's toolbar, not the sidebar column's: while the sidebar slides in the column starts with no
            // width, and an item that lives there was folded into the ">>" overflow button for a moment.
            ToolbarItem(placement: .navigation) {
                Menu {
                    Button("加入 Kubernetes 叢集") { editor = .join }
                    Button("啟動原生終端服務") { editor = .native }
                    Button("新增 Docker 容器") { editor = .docker }
                } label: { Label("新增", systemImage: "plus") }
                .help("加入 Kubernetes 叢集，或新增本機服務").accessibilityLabel("新增")
                .disabled(model.busy)
            }
            // Standard toolbar items: the system draws the glass, so no custom style or panel.
            ToolbarItemGroup(placement: .primaryAction) {
                    Menu {
                        Button("新增執行環境…", systemImage: "plus") { environmentEditor = EnvironmentEditor(editing: nil) }
                        if !model.environments.isEmpty {
                            Divider()
                            ForEach(model.environments) { environment in
                                Menu(environment.name) {
                                    Button("檢視") { model.selected = .environment(environment.name) }
                                    Button("編輯…") { environmentEditor = EnvironmentEditor(editing: environment) }
                                    Button("移除登記…", role: .destructive) { confirmRemoveEnvironment(environment) }
                                }
                            }
                        }
                    } label: { Label("執行環境", systemImage: "shippingbox.and.arrow.backward") }
                    .menuIndicator(.hidden)
                    .toolbarTooltip("執行環境").accessibilityLabel("執行環境")
                    Button("運行日誌", systemImage: "list.bullet.rectangle") { showRuntimeLog = true }.toolbarTooltip("運行日誌").accessibilityLabel("運行日誌")
                    Button("重新整理", systemImage: "arrow.clockwise") { Task { await model.refresh() } }.toolbarTooltip("重新整理").accessibilityLabel("重新整理")
                    Button("設定", systemImage: "gearshape") { model.settingsPresented = true }.toolbarTooltip("設定").accessibilityLabel("設定")
                    Button("FAQ", systemImage: "questionmark.circle") { showHelp = true }.toolbarTooltip("說明與 FAQ").accessibilityLabel("FAQ")
            }
          }
        }
        // Like Launchbit, a pop-up consumes everything: the content and the toolbar behind it stop responding.
        .disabled(modalID != nil)
        .overlay { modalLayer }
        .alert(item: $pending) { action in
            Alert(title: Text(action.title), message: Text(action.message), primaryButton: .destructive(Text("確認")) {
                Task {
                    if let request = action.request { try? await model.perform("delete", request) }
                    if let id = action.leave { try? await model.perform("leave", LeaveRequest(clusterID: id)) }
                    if let name = action.environment { await model.removeEnvironment(name) }
                }
            }, secondaryButton: .cancel(Text("取消")))
        }
        .alert("需要「區域網路」權限", isPresented: $model.localNetworkNote) {
            Button("繼續") {
                UserDefaults.standard.set(true, forKey: "localNetworkExplained")
                Task { await model.triggerLocalNetworkPrompt() }
            }
            Button("稍後", role: .cancel) { }
        } message: {
            Text("MacCluster 需要連到區域網路上的叢集控制平面。按「繼續」後，macOS 會詢問是否允許 MacCluster 使用區域網路，請選擇「允許」。若之後無法連線，可以到「設定 → 健康檢查」查看原因。")
        }
        // Starting the background service and the periodic refresh are separate tasks: if the first one waits (for the
        // agent to come up, or for a command that does not return), the refresh must still keep running.
        .task {
            if SidebarDiagnostics.enabled { SidebarDiagnostics.configure(model) }
            else { await model.initialize() }
        }
        .task {
            guard !SidebarDiagnostics.enabled else { return }
            while !Task.isCancelled { await model.refresh(); try? await Task.sleep(for: .seconds(3)) }
        }
        .onChange(of: columnVisibility) {
            if SidebarDiagnostics.enabled { SidebarDiagnostics.expanded = columnVisibility != .detailOnly }
        }
        .onChange(of: model.selected) {
            guard !SidebarDiagnostics.enabled else { return }
            if let workload = model.selectedWorkload { model.acknowledge(workload, clusterID: model.selectedClusterID) }
            Task { await model.loadLogs() }
        }
    }
    /// Which pop-up is open, so the overlay can animate between them.
    private var modalID: String? {
        if let kind = editor { return "editor-" + kind.rawValue }
        if let request = environmentEditor { return "environment-" + request.id.uuidString }
        if let w = statsWorkload { return "stats-" + w.uid }
        if let w = inspectWorkload { return "inspect-" + w.uid }
        if showRuntimeLog { return "runtime-log" }
        if model.settingsPresented { return "settings" }
        if showHelp { return "help" }
        return nil
    }
    private func closeModal() {
        editor = nil; environmentEditor = nil; statsWorkload = nil; inspectWorkload = nil
        showRuntimeLog = false; model.settingsPresented = false; showHelp = false
    }
    @ViewBuilder private var modalLayer: some View {
        if modalID != nil {
            Group {
                if let kind = editor {
                    ModalHost(label: kind == .join ? "加入 Kubernetes 叢集" : (kind == .configure ? "設定管理 kubeconfig" : "新增服務"), isDismissable: !model.busy, dismiss: closeModal) {
                        if kind == .join { JoinView() }
                        else if kind == .configure { ConfigureView(clusterID: configureClusterID) }
                        else { CreateView(executor: kind.rawValue) }
                    }
                } else if let request = environmentEditor {
                    ModalHost(label: "執行環境", isDismissable: !model.busy, dismiss: closeModal) { EnvironmentFormView(editing: request.editing) }
                } else if let workload = statsWorkload {
                    ModalHost(label: "資源使用狀況", height: 440, dismiss: closeModal) { DockerStatsView(workload: workload) }
                } else if let workload = inspectWorkload {
                    ModalHost(label: "容器詳細資訊", height: 620, dismiss: closeModal) { DockerInspectView(workload: workload) }
                } else if showRuntimeLog {
                    ModalHost(label: "運行日誌", width: 740, height: 520, dismiss: closeModal) { RuntimeLogView() }
                } else if model.settingsPresented {
                    ModalHost(label: "設定", width: 640, height: 704, minWidth: 520, minHeight: 480, isDismissable: !model.busy, dismiss: closeModal) {
                        TabbedPanel(done: closeModal) { AdvancedSettings() }
                    }
                } else if showHelp {
                    ModalHost(label: "說明與 FAQ", width: 640, height: 704, minWidth: 520, minHeight: 480, dismiss: closeModal) {
                        TabbedPanel(done: closeModal) { HelpView() }
                    }
                }
            }
        }
    }
    private var statusHeader: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text(model.selectedCluster?.name ?? model.selectedCompose?.id ?? model.selectedEnvironment.map { "執行環境 · " + $0.name } ?? (model.selectedWorkload != nil ? "本機服務" : "MacCluster")).font(.title.bold())
                Spacer()
                if model.busy { ProgressView().controlSize(.small) }
                if let updated = model.lastUpdated { Text("最後更新時間：\(updated.formatted(date: .omitted, time: .standard))").font(.caption).foregroundStyle(.secondary) }
            }

            if model.error != nil {
                Label(model.status == nil ? "無法與管理程式連線，請至設定啟動管理程式。" : "無法與管理程式連線，顯示上次取得的資料。", systemImage: "exclamationmark.triangle.fill")
                    .foregroundStyle(.orange)
            }
            if let error = AgentErrorPresentation.message(model.startupError) { Label(error, systemImage: "server.rack").foregroundStyle(.orange) }
            if let error = model.inventoryError { Label(error, systemImage: "shippingbox").foregroundStyle(.orange) }
            if let error = AgentErrorPresentation.message(model.environmentError) { Label(error, systemImage: "shippingbox.and.arrow.backward").foregroundStyle(.orange) }
            if let error = AgentErrorPresentation.message(model.actionError ?? model.joinError) { Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red).textSelection(.enabled) }
            if let notice = model.notice ?? model.joinNotice ?? model.startupNotice?.text {
                Text(notice).font(.caption).foregroundStyle(.secondary)
            }
        }
    }
    private func clusterConnection(_ cluster: Cluster) -> String {
        if model.error != nil { return "叢集狀態尚未更新" }
        switch cluster.phase {
        case "Ready": return "叢集已連線"
        case "Joining": return "正在加入叢集"
        case "Leaving": return "正在退出叢集"
        case "Removed": return "節點已從叢集移除"
        default: return "叢集連線異常"
        }
    }
    private func runtimeStatus(_ workload: Workload) -> String {
        let unavailable = workload.isExternal ? model.inventoryError != nil : model.error != nil
        return unavailable ? "本機狀態無法更新 · 上次為 " + workload.phase : "本機執行狀態：" + workload.phase
    }
    /// A stale answer cannot be called running or failed, so it gets the "unknown" colour.
    private func runtimeTone(_ workload: Workload) -> StatusTone {
        let unavailable = workload.isExternal ? model.inventoryError != nil : model.error != nil
        return unavailable ? .unknown : StatusStyle.tone(phase: workload.phase)
    }
    private func expansion(_ selection: Selection) -> Binding<Bool> {
        Binding(get: { expandedGroups.contains(selection) }, set: { expanded in
            if expanded { expandedGroups.insert(selection) } else { expandedGroups.remove(selection) }
        })
    }
    private func toggleExpansion(_ selection: Selection) {
        let expanded = expansion(selection)
        expanded.wrappedValue.toggle()
    }
    private func composeTone(_ project: ComposeProject) -> StatusTone {
        model.inventoryError != nil ? .unknown : StatusStyle.tone(compose: project.workloads.map(\.phase))
    }
    private func clusterTone(_ cluster: Cluster) -> StatusTone {
        model.error != nil ? .unknown : StatusStyle.tone(phase: cluster.phase)
    }
    private func workloadRow(_ workload: Workload, clusterID: String) -> some View {
        Button {
            model.selected = .workload(clusterID, workload.uid)
            model.acknowledge(workload, clusterID: clusterID)
        } label: {
        VStack(alignment: .leading, spacing: 3) {
            Label(workload.name, systemImage: workload.executor == "native" ? "terminal" : "shippingbox")
            StatusText(text: runtimeStatus(workload), tone: runtimeTone(workload))
            if model.hasUnreadFailure(workload, clusterID: clusterID) { Label("未讀異常", systemImage: "circle.fill").font(.caption).foregroundStyle(.orange) }
            if workload.isExternal { Text("本機 Docker 容器").font(.caption).foregroundStyle(.secondary) }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .contentShape(Rectangle())
        .padding(6)
        .background {
            if model.selected == .workload(clusterID, workload.uid) { SelectionHighlight() }
        }
        }
        .buttonStyle(.plain)
        .sidebarHover()
        .accessibilityAddTraits(model.selected == .workload(clusterID, workload.uid) ? .isSelected : [])
        .contextMenu { deleteButtons(workload, clusterID: clusterID) }
    }
    private func confirmRemoveEnvironment(_ environment: EnvStatus) {
        let users = (model.locals + model.clusters.flatMap { $0.workloads ?? [] }).filter { $0.environment == environment.name }
        let warning = users.isEmpty ? "" : "\n\n目前有 \(users.count) 個工作使用它（\(users.map(\.name).joined(separator: "、"))）。它們的現有程序會繼續執行，但之後重新啟動會因找不到環境而停在 Pending。"
        pending = PendingAction(title: "移除執行環境 \(environment.name)？", message: "只移除 MacCluster 的登記並撤下 Node label，不會刪除或修改 \(environment.root)。\(warning)", request: nil, leave: nil, environment: environment.name)
    }
    private func composeDetail(_ project: ComposeProject) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            StatusBadge(text: "Docker Compose · \(project.workloads.count) 個容器 · \(project.runningCount) 個執行中", tone: composeTone(project))
            if let first = project.workloads.first {
                GroupBox {
                    VStack(alignment: .leading, spacing: 8) {
                        ComposeRows(project: project.id, service: nil, directory: first.composeWorkingDir, files: first.composeConfigFiles)
                        Text("顯示容器建立時的 Compose 標籤；原始檔案可能已移動或變更。")
                            .font(.caption).foregroundStyle(.secondary)
                    }.frame(maxWidth: .infinity, alignment: .leading)
                } label: { Label("專案資訊", systemImage: "square.stack.3d.up") }
            }
            Text("選取下方容器可查看詳細資訊、資源使用狀況與啟停操作。")
                .font(.caption).foregroundStyle(.secondary)
            ForEach(project.workloads) { workload in
                Button {
                    model.selected = .workload("", workload.uid)
                } label: {
                    HStack {
                        Image(systemName: "shippingbox")
                        VStack(alignment: .leading, spacing: 4) {
                            Text(workload.name).font(.headline)
                            Text("服務：\(workload.composeService ?? "—")").font(.caption)
                        }
                        Spacer()
                        StatusText(text: runtimeStatus(workload), tone: runtimeTone(workload))
                        Image(systemName: "chevron.right")
                    }
                    .padding(12)
                    .contentCard()
                    .contentShape(Rectangle())
                }.buttonStyle(.plain)
            }
        }
    }
    private func clusterDetail(_ cluster: Cluster) -> some View {
        VStack(alignment: .leading, spacing: 14) {
            GroupBox {
                VStack(alignment: .leading, spacing: 10) {
                    InfoRow(label: "狀態") { StatusBadge(text: clusterConnection(cluster), tone: clusterTone(cluster)) }
                    InfoRow(label: "Endpoint") { Text(cluster.endpoint).font(.system(.body, design: .monospaced)).textSelection(.enabled) }
                    InfoRow(label: "節點") { Text(cluster.nodeName).font(.system(.body, design: .monospaced)).textSelection(.enabled) }
                    InfoRow(label: "此 Mac 的工作") { Text("\(cluster.workloads?.count ?? 0) 個（僅顯示指派至此 Mac 的工作，不是叢集全部）") }
                    InfoRow(label: "管理權限") {
                        Label(cluster.managementConfigured ? "已設定管理 kubeconfig" : "尚未設定",
                              systemImage: cluster.managementConfigured ? "checkmark.seal" : "exclamationmark.triangle")
                            .foregroundStyle(cluster.managementConfigured ? Color.green : Color.orange)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            } label: {
                Label("連線資訊", systemImage: "server.rack")
            }
            if let message = cluster.message, !message.isEmpty {
                ClusterNote(warning: cluster.phase != "Ready", title: "節點回報", text: message)
            }
            ClusterNote(warning: !cluster.managementConfigured, title: nil,
                        text: cluster.managementConfigured ? "實際權限由 API 驗證。" : "刪除工作控制器或離開叢集可能需要管理權限。")
            HStack {
                Button("設定管理 kubeconfig…") { configureClusterID = cluster.id; editor = .configure }
                Button("離開此叢集…", role: .destructive) { confirmLeave(cluster) }
            }.glassControls().disabled(model.busy)
        }
    }
    private func workloadDetail(_ workload: Workload) -> some View {
        GeometryReader { geo in
            // The page opens at its top: the details first, the log below them (the cards above can be folded).
            ScrollView(.vertical) {
                workloadDetailContent(workload, viewportHeight: geo.size.height)
                    .frame(maxWidth: .infinity, alignment: .topLeading)
            }
        }
    }
    /// PID only when there is one; ports have their own card.
    private func runtimeFacts(_ workload: Workload) -> String? {
        guard let pid = workload.pid, pid > 0 else { return nil }
        return "PID：\(pid)"
    }
    private func workloadDetailContent(_ workload: Workload, viewportHeight: CGFloat) -> some View {
        VStack(alignment: .leading, spacing: 12) {
            Text(workload.name).font(.title.bold())
            Text(workload.isExternal ? "本機 Docker 容器 · 手動啟停" : (model.selectedClusterID.isEmpty ? "本機服務 · \(workload.executor)" : "此 Mac 的叢集工作 · \(workload.namespace) · \(workload.executor)")).foregroundStyle(.secondary)
            HStack(spacing: 14) {
                StatusBadge(text: runtimeStatus(workload), tone: runtimeTone(workload))
                if let facts = runtimeFacts(workload) { Text(facts) }
            }
            if let cluster = model.selectedCluster {
                StatusText(text: clusterConnection(cluster), tone: clusterTone(cluster))
                if cluster.phase != "Ready" || model.error != nil {
                    Text("本機程序狀態與叢集連線分開顯示；連線恢復前，無法確認最新調度要求與狀態回報。").font(.caption).foregroundStyle(.secondary)
                }
            }
            if workload.executor == "docker" { ContainerInfoCard(workload: workload) }
            PortsCard(workload: workload)
            if workload.isExternal {
                if model.inventoryError != nil { Text("本機容器資料已過期").foregroundStyle(.orange) }
                HStack {
                    Button("啟動", systemImage: "play.fill") { Task { await model.controlDocker(workload, action: "start") } }
                        .disabled(model.busy || model.inventoryError != nil || !["Stopped", "Created"].contains(workload.phase))
                    Button("停止", systemImage: "stop.fill") { Task { await model.controlDocker(workload, action: "stop") } }
                        .disabled(model.busy || model.inventoryError != nil || !["Running", "Restarting"].contains(workload.phase))
                    Button("複製容器 ID") {
                        if let id = workload.containerID {
                            NSPasteboard.general.clearContents()
                            NSPasteboard.general.setString(id, forType: .string)
                        }
                    }
                }
                .glassControls()

            }
            if workload.executor == "docker", let id = workload.containerID, !id.isEmpty {
                HStack {
                    Button("容器詳細資訊", systemImage: "doc.text.magnifyingglass") { inspectWorkload = workload }
                    Button("資源使用狀況", systemImage: "chart.bar") { statsWorkload = workload }
                }
                .glassControls()
            }
            if workload.phase == "Unknown" { Text("執行器無法確認目前狀態，不能視為正常或已退出。").foregroundStyle(.orange) }
            if let message = workload.message, !message.isEmpty { Text(message).textSelection(.enabled) }
            HStack { deleteButtons(workload, clusterID: model.selectedClusterID); Spacer(); Button("讀取日誌") { Task { await model.loadLogs() } } }.glassControls().disabled(model.busy)
            HStack {
                Text("日誌").font(.headline)
                Spacer()
                Button("頂端") { logScrollAction = -(abs(logScrollAction) + 1) }
                Button("底部") { logScrollAction = abs(logScrollAction) + 1 }
            }
            .glassControls()
            LogPanel(viewport: viewportHeight,
                     text: model.logError ?? model.logs,
                     identity: model.selectedClusterID + ":" + workload.uid,
                     scrollAction: logScrollAction)
                .id("log")

        }
    }
    @ViewBuilder private func deleteButtons(_ workload: Workload, clusterID: String) -> some View {
        if !workload.isExternal {
        Button(clusterID.isEmpty ? "永久刪除本機服務…" : "刪除這個副本…", role: .destructive) { confirmDelete(workload, clusterID: clusterID, mode: "pod") }
        if !clusterID.isEmpty {
            Button("刪除所屬工作…", role: .destructive) { confirmDelete(workload, clusterID: clusterID, mode: "workload") }
        }
    }
    }
    private func confirmDelete(_ workload: Workload, clusterID: String, mode: String) {
        let detail = clusterID.isEmpty ? "停止並永久移除本機服務定義與其執行資源。之後不會自動重新啟動。" : (mode == "pod" ? "只刪除這個 Pod；Deployment 等控制器可能立即建立替代副本。" : "刪除此 Pod 所屬的 Deployment／StatefulSet／DaemonSet／Job（或獨立 Pod），會影響該工作在所有節點上的副本。需要具權限的管理 kubeconfig。")
        pending = PendingAction(title: "刪除 \(workload.name)？", message: "\(clusterID.isEmpty ? "本機" : "叢集 " + clusterID) · \(workload.namespace)\nUID：\(workload.uid)\n\n\(detail)", request: DeleteRequest(uid: workload.uid, clusterID: clusterID, mode: mode), leave: nil)
    }
    private func confirmLeave(_ cluster: Cluster) {
        pending = PendingAction(title: "離開 \(cluster.name)？", message: "\(cluster.endpoint)\nNode：\(cluster.nodeName)\n\n封鎖此節點的新排程、停止此叢集在 Mac 的工作，移除 Node 與本機連線設定。其他叢集與本機服務不受影響。遠端清理失敗會保留設定供重試；不會刪除整個叢集。", request: nil, leave: cluster.id)
    }
}
struct RuntimeLogEntry: Identifiable {
    let scope: String
    let clusterID: String
    let workload: Workload
    var id: String { clusterID + ":" + workload.uid }
}
struct RuntimeLogView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.modalDismiss) var dismiss
    private var entries: [RuntimeLogEntry] {
        let local = model.locals.filter { !$0.isExternal && $0.hasFailure }.map {
            RuntimeLogEntry(scope: "本機服務", clusterID: "", workload: $0)
        }
        let clustered = model.clusters.flatMap { cluster in
            (cluster.workloads ?? []).filter(\.hasFailure).map {
                RuntimeLogEntry(scope: cluster.name, clusterID: cluster.id, workload: $0)
            }
        }
        return (local + clustered).sorted {
            ($0.workload.lastExitAt ?? "") > ($1.workload.lastExitAt ?? "")
        }
    }
    private var events: [KubernetesEvent] {
        (model.status?.events ?? []).sorted { $0.time > $1.time }
    }
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                Text("運行日誌").font(.title2.bold())
                Spacer()
                Button("重新整理") { Task { await model.refresh() } }
                Button("完成") { dismiss() }.keyboardShortcut(.cancelAction)
            }.padding(12).sheetPanel()
            Text("記錄此 Mac 的叢集加入、離開與 Pod 指派／狀態變更，另保留服務失敗摘要。事件自新版管理程式啟用後開始保存。")
                .font(.caption).foregroundStyle(.secondary)
            if model.error != nil { Text("管理程式未連線，以下為上次取得的紀錄。").foregroundStyle(.secondary) }
            if events.isEmpty && entries.isEmpty && !model.clusters.contains(where: { $0.phase == "Removed" }) {
                ContentUnavailableView("尚無運行紀錄", systemImage: "list.bullet.rectangle", description: Text("叢集與 Pod 事件，以及服務失敗紀錄，會顯示在這裡。"))
                    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .center)
            } else {
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 14) {
                        ForEach(events) { event in KubernetesEventRow(event: event) }
                        ForEach(model.clusters.filter { $0.phase == "Removed" }) { cluster in
                            GroupBox(cluster.name) {
                                Text(cluster.message ?? "節點已從叢集移除")
                                    .frame(maxWidth: .infinity, alignment: .leading).textSelection(.enabled)
                            }
                        }
                        if !entries.isEmpty { Text("服務失敗摘要").font(.headline) }
                        ForEach(entries) { entry in
                            let w = entry.workload
                            GroupBox {
                                VStack(alignment: .leading, spacing: 6) {
                                    Text(w.name).font(.headline)
                                    Text("\(entry.scope) · \(w.namespace) · \(w.executor)").foregroundStyle(.secondary)
                                    Text("最近回報狀態：\(w.phase) · 失敗 \(w.failureCount ?? 0) 次 · 重啟 \(w.restarts ?? 0) 次")
                                    if let date = w.lastExitAt, !date.isEmpty {
                                        Text("最後退出：\(date) · exit code：\(w.lastExitCode.map(String.init) ?? "—")")
                                    }
                                    if let reason = w.lastExitReason, !reason.isEmpty { Text(reason) }
                                    if w.phase == "Failed", let message = w.message, !message.isEmpty { Text(message) }
                                    if let cluster = model.clusters.first(where: { $0.id == entry.clusterID }), cluster.phase != "Ready" {
                                        Text("叢集連線異常；上列執行狀態由本機觀察，調度與回報可能尚未同步。").font(.caption).foregroundStyle(.secondary)
                                    }
                                }.frame(maxWidth: .infinity, alignment: .leading).textSelection(.enabled)
                            }
                        }
                    }
                }
            }
        }.padding(24).frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading).appScreen()
    }
}

struct JoinView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.modalDismiss) var dismiss
    @State private var command = ""
    @State private var name = ""
    @State private var proxyURL = ""
    @State private var management = ""
    @State private var nodeName = ""
    @State private var error: String?
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("加入 Kubernetes 叢集").font(.title2.bold()).sheetHeading()
            Text("平台只解析 kubeadm join 參數，不會執行貼上的 shell 文字。")
            TextField("叢集顯示名稱（選填）", text: $name)
            HStack {
                TextField("Node 名稱（選填，自動產生）", text: $nodeName)
                Button("預設") {
                    nodeName = JoinValidation.defaultNodeName(hostName: ProcessInfo.processInfo.hostName)
                    error = nil
                }
                .help("填入以這台 Mac 的主機名稱產生的預設 Node 名稱")
                .disabled(model.busy)
            }
            Text("可自行命名，無固定前綴限制；留空時自動產生。名稱須符合 Kubernetes 的小寫 DNS 名稱格式。")
                .font(.caption).foregroundStyle(.secondary)
            TextField("kubeadm join … --token … --discovery-token-ca-cert-hash …", text: $command)
            TextField("代理 URL（選填，不含帳密）", text: $proxyURL)
            TextField("管理 kubeconfig 絕對路徑（選填）", text: $management)
            Text("Bootstrap token 不代表管理權限。Token 不會儲存或放入程序參數；代理設定會儲存。管理 kubeconfig 僅保存路徑。隧道須另外維持運作。")
                .font(.caption).foregroundStyle(.secondary)
            if let error { Text(error).foregroundStyle(.red) }
            HStack {
                Spacer(); Button("取消") { command = ""; dismiss() }.keyboardShortcut(.cancelAction).disabled(model.busy)
                Button(model.busy ? "送出中…" : "開始加入") {
                    Task {
                        do {
                            try JoinValidation.validateNodeName(nodeName)
                            guard management.isEmpty || management.hasPrefix("/") else { throw ClientError.message("管理 kubeconfig 須為絕對路徑。") }
                            try await model.perform("join", JoinRequest(command: command, nodeName: nodeName, proxyURL: proxyURL, name: name, managementKubeconfig: management), sensitive: true)
                            command = ""; dismiss()
                        } catch { self.error = error.localizedDescription }
                    }
                }.glassPrimaryAction().disabled(command.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || model.busy)
            }
        }.padding(24).frame(width: 580).appScreen()
    }
}
struct ConfigureView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.modalDismiss) var dismiss
    let clusterID: String
    @State private var path = ""
    @State private var error: String?
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("設定管理 kubeconfig").font(.title2.bold()).sheetHeading()
            Text("叢集：\(model.clusters.first { $0.id == clusterID }?.name ?? clusterID)")
            Text("用於刪除工作控制器及遠端 Node 清理。平台會驗證 API endpoint、CA 與連線，僅儲存檔案路徑，不複製管理憑證。")
            TextField("/absolute/path/to/kubeconfig", text: $path)
            if let error { Text(error).foregroundStyle(.red) }
            HStack {
                Spacer(); Button("取消") { dismiss() }.keyboardShortcut(.cancelAction).disabled(model.busy)
                Button(model.busy ? "驗證中…" : "驗證並儲存") {
                    Task {
                        do { try await model.perform("configure", ConfigureRequest(clusterID: clusterID, managementKubeconfig: path)); dismiss() }
                        catch { self.error = error.localizedDescription }
                    }
                }.disabled(!path.hasPrefix("/") || model.busy)
            }
        }.padding(24).frame(width: 560).appScreen()
    }
}
struct CreateView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.modalDismiss) var dismiss
    let executor: String
    @State private var name = ""
    @State private var image = ""
    @State private var executable = ""
    @State private var arguments = ""
    @State private var jsonArguments = false
    @State private var environment = ""
    @State private var directory = ""
    @State private var port = ""
    @State private var restart = "Never"
    @State private var shellMode = false
    @State private var selectedEnvironment = ""
    @State private var error: String?
    private var native: Bool { executor == "native" }
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text(native ? "啟動原生終端服務" : "新增 Docker 容器").font(.title2.bold()).sheetHeading()
            Form {
                TextField("服務名稱", text: $name)
                if native {
                    Toggle("明確使用 shell（/bin/zsh -lc）", isOn: $shellMode)
                    if shellMode { TextField("Shell 指令", text: $executable) }
                    else { TextField("執行檔絕對路徑", text: $executable) }
                    TextField("工作目錄（選填，絕對路徑）", text: $directory)
                    Picker("執行環境（選填）", selection: $selectedEnvironment) {
                        Text("不使用").tag("")
                        ForEach(model.environments.filter(\.available)) { Text($0.name).tag($0.name) }
                    }
                    if !selectedEnvironment.isEmpty { Text("選取環境後，執行檔可只寫該環境 bin 目錄內的指令名稱（例如 vllm），或仍使用絕對路徑。").font(.caption).foregroundStyle(.secondary) }
                } else {
                    TextField("Docker image", text: $image)
                    TextField("容器 command（選填，單一執行檔）", text: $executable)
                }
                if !shellMode {
                    Toggle("args 使用 JSON 字串陣列", isOn: $jsonArguments)
                    Text(jsonArguments ? "例如 [\"--port\", \"8080\"]；不執行 shell 展開" : "args：每行一個參數；不需要 shell 引號")
                        .font(.caption).foregroundStyle(.secondary)
                    TextEditor(text: $arguments).font(.system(.body, design: .monospaced)).frame(height: 70).border(.secondary.opacity(0.3))
                }
                TextField("\(native ? "服務" : "容器")埠（選填，1–65535）", text: $port)
                Text("環境變數：每行 KEY=VALUE").font(.caption)
                TextEditor(text: $environment).font(.system(.body, design: .monospaced)).frame(height: 60).border(.secondary.opacity(0.3))
                Picker("重啟策略", selection: $restart) { Text("Never（預設）").tag("Never"); Text("OnFailure").tag("OnFailure"); Text("Always").tag("Always") }
            }
            Text("服務定義與環境變數將儲存在受保護的本機狀態目錄，背景服務重啟後仍保留。原生指令以 \(NSUserName()) 執行，可存取該帳號的主機資源。")
                .font(.caption).foregroundStyle(.secondary)
            if let error { Text(error).foregroundStyle(.red).textSelection(.enabled) }
            HStack {
                Spacer(); Button("取消") { dismiss() }.keyboardShortcut(.cancelAction).disabled(model.busy)
                Button(model.busy ? "建立中…" : "建立並啟動") {
                    Task {
                        do { try await model.perform("create", validatedRequest()); dismiss() }
                        catch { self.error = error.localizedDescription }
                    }
                }.glassPrimaryAction().disabled(model.busy || name.trimmingCharacters(in: .whitespaces).isEmpty)
            }
        }.padding(24).frame(width: 620).appScreen()
    }
    private func validatedRequest() throws -> CreateRequest {
        func invalid(_ text: String) -> ClientError { .message(text) }
        guard !native || shellMode || executable.hasPrefix("/") || (!selectedEnvironment.isEmpty && !executable.isEmpty && !executable.contains("/")) else { throw invalid("原生執行檔須為絕對路徑。") }
        guard !shellMode || !executable.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else { throw invalid("Shell 指令不可空白。") }
        guard native || !image.trimmingCharacters(in: .whitespaces).isEmpty else { throw invalid("請輸入 Docker image。") }
        guard directory.isEmpty || directory.hasPrefix("/") else { throw invalid("工作目錄須為絕對路徑。") }
        let portValue: Int
        if port.isEmpty { portValue = 0 }
        else if let value = Int(port), (1...65535).contains(value) { portValue = value }
        else { throw invalid("埠必須是 1–65535 的整數。") }
        let args: [String]
        if shellMode { args = ["-lc", executable] }
        else if jsonArguments {
            guard let data = arguments.data(using: .utf8), let values = try? JSONDecoder().decode([String].self, from: data) else { throw invalid("args 必須是有效 JSON 字串陣列，例如 [\"--port\", \"8080\"]。") }
            args = values
        } else { args = arguments.isEmpty ? [] : arguments.components(separatedBy: "\n") }
        var env: [String: String] = [:]
        for line in environment.components(separatedBy: "\n") where !line.isEmpty {
            guard let equal = line.firstIndex(of: "="), equal != line.startIndex else { throw invalid("環境變數每行必須為 KEY=VALUE。") }
            let key = String(line[..<equal])
            guard !key.contains(where: { $0.isWhitespace }), env[key] == nil else { throw invalid("環境變數名稱不得含空白或重複。") }
            env[key] = String(line[line.index(after: equal)...])
        }
        return CreateRequest(name: name, executor: executor, image: image, command: shellMode ? ["/bin/zsh"] : (executable.isEmpty ? [] : [executable]), args: args, env: env, workingDir: directory, containerPort: portValue, restartPolicy: restart, environment: native ? selectedEnvironment : "")
    }
}
enum HelpTopic: String, CaseIterable {
    case general = "一般與管理"
    case containers = "容器與日誌"
    case native = "原生執行"
    case cluster = "叢集與副本"
    case network = "網路限制"
    case yaml = "套用 YAML"
}

struct HelpView: View {
    @State private var topic = HelpTopic.general.rawValue
    var body: some View {
        AnimatedTabView(pages: [
            .init(id: HelpTopic.general.rawValue, title: HelpTopic.general.rawValue, content: AnyView(GeneralHelpPage())),
            .init(id: HelpTopic.containers.rawValue, title: HelpTopic.containers.rawValue, content: AnyView(ContainersHelpPage())),
            .init(id: HelpTopic.native.rawValue, title: HelpTopic.native.rawValue, content: AnyView(NativeHelpPage())),
            .init(id: HelpTopic.cluster.rawValue, title: HelpTopic.cluster.rawValue, content: AnyView(ClusterHelpPage())),
            .init(id: HelpTopic.network.rawValue, title: HelpTopic.network.rawValue, content: AnyView(NetworkHelpPage())),
            .init(id: HelpTopic.yaml.rawValue, title: HelpTopic.yaml.rawValue, content: AnyView(YAMLHelpPage()))
        ], selection: $topic)
    }
}
