import SwiftUI
import AppKit
import SwiftTerm

/// One host-local execution environment as reported by `maccluster-agent env list --json`.
struct EnvStatus: Decodable, Identifiable, Sendable, Hashable {
    let name: String
    let type: String
    let root: String
    let binDir: String
    let workDir: String?
    let extraPath: [String]?
    let env: [String: String]?
    let available: Bool
    let problem: String?
    let executables: [String]?
    var id: String { name }
    var nodeLabel: String { "env.maccluster.example.com/" + name }
    var labelSnippet: String {
        "labels:\n  maccluster.example.com/executor: native\n  maccluster.example.com/environment: \(name)\n" +
        "nodeSelector:\n  \(nodeLabel): \"true\""
    }
}

struct ShellSpec: Decodable, Sendable {
    let executable: String
    let args: [String]
    let env: [String]
    let cwd: String
}

/// Embedded terminals live here, not in the view, so switching the sidebar
/// selection does not kill a running shell.
@MainActor final class TerminalSessions: NSObject, ObservableObject, LocalProcessTerminalViewDelegate {
    @Published private(set) var running: Set<String> = []
    @Published var lastError: String?
    private var views: [String: LocalProcessTerminalView] = [:]
    private var writers: [String: TerminalLogWriter] = [:]

    func view(for name: String) -> LocalProcessTerminalView? { views[name] }
    /// Makes sure what the open terminal printed so far is on disk before the log is read.
    func flushLog(_ name: String) { writers[name]?.flush() }

    func open(_ name: String) async {
        guard views[name] == nil else { return }
        lastError = nil
        do {
            let data = try await AgentClient.command(["env", "shell", "--name", name])
            open(name, spec: try JSONDecoder().decode(ShellSpec.self, from: data))
        } catch { lastError = error.localizedDescription }
    }

    func open(_ name: String, spec: ShellSpec) {
        guard views[name] == nil else { return }
        let view = LoggedTerminalView(frame: NSRect(x: 0, y: 0, width: 640, height: 320))
        let writer = TerminalLogWriter(state: AgentClient.stateDirectory, environment: name, shell: spec.executable)
        view.logWriter = writer
        writers[name] = writer
        view.processDelegate = self
        view.font = NSFont.monospacedSystemFont(ofSize: 13, weight: .regular)
        view.startProcess(executable: spec.executable, args: spec.args, environment: spec.env, execName: nil, currentDirectory: spec.cwd)
        views[name] = view
        running.insert(name)
    }

    func close(_ name: String) {
        views[name]?.terminate()
        views[name] = nil
        writers.removeValue(forKey: name)?.close()
        running.remove(name)
    }

    private func remove(_ id: ObjectIdentifier) {
        guard let name = views.first(where: { ObjectIdentifier($0.value) == id })?.key else { return }
        views[name] = nil
        writers.removeValue(forKey: name)?.close()
        running.remove(name)
    }

    nonisolated func sizeChanged(source: LocalProcessTerminalView, newCols: Int, newRows: Int) {}
    nonisolated func setTerminalTitle(source: LocalProcessTerminalView, title: String) {}
    nonisolated func hostCurrentDirectoryUpdate(source: TerminalView, directory: String?) {}
    nonisolated func processTerminated(source: TerminalView, exitCode: Int32?) {
        let id = ObjectIdentifier(source)
        Task { @MainActor in self.remove(id) }
    }
}

struct EnvTerminalHost: NSViewRepresentable {
    let view: LocalProcessTerminalView
    func makeNSView(context: Context) -> LocalProcessTerminalView { view }
    func updateNSView(_ nsView: LocalProcessTerminalView, context: Context) {}
}

/// Attached to the window as a sheet, like Launchbit does, rather than a free-floating modal panel.
@MainActor func pickDirectory(_ start: String = "", completion: @escaping (String?) -> Void) {
    let panel = NSOpenPanel()
    panel.canChooseDirectories = true
    panel.canChooseFiles = false
    panel.allowsMultipleSelection = false
    panel.showsHiddenFiles = true
    if !start.isEmpty { panel.directoryURL = URL(fileURLWithPath: start) }
    if let window = NSApp.keyWindow ?? NSApp.mainWindow {
        panel.beginSheetModal(for: window) { response in completion(response == .OK ? panel.url?.path : nil) }
    } else {
        completion(panel.runModal() == .OK ? panel.url?.path : nil)
    }
}

struct EnvironmentDetailView: View {
    @EnvironmentObject var model: AppModel
    @ObservedObject var terminals: TerminalSessions
    let environment: EnvStatus
    let edit: () -> Void
    let remove: () -> Void
    @State private var showExecutables = false
    @State private var terminalLog = ""
    @State private var terminalLogScroll = 0

    private var users: [Workload] {
        (model.locals + model.clusters.flatMap { $0.workloads ?? [] }).filter { $0.environment == environment.name }
    }
    private var terminalOpen: Bool { terminals.running.contains(environment.name) }
    private func reloadTerminalLog() async {
        let name = environment.name
        terminals.flushLog(name)
        let text = await Task.detached { TerminalLog.read(AgentClient.stateDirectory, name) }.value
        if text != terminalLog { terminalLog = text }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            HStack {
                Label(environment.name, systemImage: "shippingbox.and.arrow.backward").font(.title.bold())
                Spacer()
                Text(environment.available ? "可用" : "無法使用")
                    .font(.caption.bold()).padding(.horizontal, 10).padding(.vertical, 4)
                    .background(environment.available ? Color.green.opacity(0.25) : Color.orange.opacity(0.3), in: Capsule())
            }
            HStack {
                Button(terminalOpen ? "關閉終端" : "開啟終端", systemImage: "terminal") {
                    if terminalOpen { terminals.close(environment.name) } else { Task { await terminals.open(environment.name) } }
                }.disabled(!environment.available)
                Button("在 Finder 顯示", systemImage: "folder") {
                    NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: environment.root)])
                }
                Button("編輯…", systemImage: "pencil", action: edit)
                Button("移除登記…", systemImage: "trash", role: .destructive, action: remove).disabled(model.busy)
                Spacer()
            }.glassControls()
            if let error = terminals.lastError { Label(error, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.red).textSelection(.enabled) }
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    if let problem = environment.problem, !environment.available {
                        Label(problem, systemImage: "exclamationmark.triangle").foregroundStyle(.orange).textSelection(.enabled)
                        Text("此環境目前不會公布為 Node label，指定它的新工作會維持 Pending。").font(.caption).foregroundStyle(.secondary)
                    }
                    GroupBox("終端日誌") {
                        VStack(alignment: .leading, spacing: 6) {
                            Label("終端輸出可能含祕密（例如印出的金鑰或權杖）。檔案只有本帳號可讀，保留最近 1 MiB 與上一次終端；環境移除時刪除，關閉後超過 7 天自動清除。", systemImage: "lock.shield")
                                .font(.caption).foregroundStyle(.secondary)
                            HStack {
                                Button("頂端") { terminalLogScroll = -(abs(terminalLogScroll) + 1) }
                                Button("底部") { terminalLogScroll = abs(terminalLogScroll) + 1 }
                                Spacer()
                                Button("清除終端日誌", role: .destructive) {
                                    TerminalLog.delete(AgentClient.stateDirectory, environment.name); terminalLog = ""
                                }.disabled(terminalOpen || terminalLog.isEmpty)
                            }.glassControls()
                            NativeLogView(text: terminalLog.isEmpty ? "尚無終端輸出。開啟終端並執行指令後，輸出會保存在這裡。" : terminalLog,
                                          identity: "terminal:" + environment.name, scrollAction: terminalLogScroll)
                                .frame(maxWidth: .infinity).frame(height: 240)
                        }.frame(maxWidth: .infinity, alignment: .leading)
                    }
                    GroupBox("設定") {
                        Grid(alignment: .leading, horizontalSpacing: 14, verticalSpacing: 6) {
                            row("類型", environment.type == "link" ? "連結既有環境" : "一般目錄")
                            row("根目錄", environment.root)
                            row("Bin 目錄", environment.binDir)
                            if let extra = environment.extraPath, !extra.isEmpty { row("額外 PATH", extra.joined(separator: "\n")) }
                            row("工作目錄", environment.workDir ?? "（未設定，終端使用根目錄）")
                            row("環境變數", (environment.env ?? [:]).isEmpty ? "（無）" : (environment.env ?? [:]).sorted { $0.key < $1.key }.map { "\($0.key)=\($0.value)" }.joined(separator: "\n"))
                        }.frame(maxWidth: .infinity, alignment: .leading).textSelection(.enabled)
                    }
                    GroupBox("在 Deployment 使用") {
                        VStack(alignment: .leading, spacing: 6) {
                            Text(environment.labelSnippet).font(.system(.caption, design: .monospaced)).textSelection(.enabled)
                            Text("command 可直接寫此環境 bin 目錄內的指令名稱（例如 `vllm`）。Pod label 放在 spec.template.metadata.labels。")
                                .font(.caption).foregroundStyle(.secondary)
                            Button("複製範例") {
                                NSPasteboard.general.clearContents()
                                NSPasteboard.general.setString(environment.labelSnippet, forType: .string)
                            }.glassControls()
                        }.frame(maxWidth: .infinity, alignment: .leading)
                    }
                    GroupBox("使用中的工作（\(users.count)）") {
                        VStack(alignment: .leading, spacing: 4) {
                            if users.isEmpty { Text("目前沒有工作使用此環境。").foregroundStyle(.secondary) }
                            ForEach(users) { w in Text("• \(w.name) · \(w.namespace) · \(w.phase)\(w.pid.map { " · PID \($0)" } ?? "")") }
                        }.frame(maxWidth: .infinity, alignment: .leading)
                    }
                    if let names = environment.executables, !names.isEmpty {
                        DisclosureGroup("Bin 目錄內的指令（\(names.count)\(names.count >= 60 ? "+" : "")）", isExpanded: $showExecutables) {
                            Text(names.joined(separator: "  ")).font(.system(.caption, design: .monospaced))
                                .frame(maxWidth: .infinity, alignment: .leading).textSelection(.enabled)
                        }
                    }
                }
            }
            .frame(maxHeight: terminalOpen ? 230 : .infinity)
            if terminalOpen, let view = terminals.view(for: environment.name) {
                VStack(alignment: .leading, spacing: 4) {
                    Text("終端 · \(environment.name)").font(.headline)
                    EnvTerminalHost(view: view)
                        .frame(maxWidth: .infinity, maxHeight: .infinity)
                        .clipShape(RoundedRectangle(cornerRadius: 8))
                    Text("終端由本應用程式內執行，以目前 macOS 帳號的權限運作；關閉分頁或視窗不會結束它，按「關閉終端」才會。")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        }
        .task(id: environment.name) {
            while !Task.isCancelled {
                await reloadTerminalLog()
                try? await Task.sleep(for: .seconds(terminalOpen ? 2 : 6))
            }
        }
    }
    private func row(_ label: String, _ value: String) -> some View {
        GridRow(alignment: .top) {
            Text(label).foregroundStyle(.secondary)
            Text(value).frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

/// The presentation and its payload change atomically, including the first menu edit.
struct EnvironmentEditor: Identifiable {
    let id = UUID()
    let editing: EnvStatus?
}

struct EnvironmentFormView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.modalDismiss) var dismiss
    let editing: EnvStatus?
    @State private var name = ""
    @State private var type = "link"
    @State private var root = ""
    @State private var binDir = ""
    @State private var workDir = ""
    @State private var extraPath = ""
    @State private var variables = ""
    @State private var error: String?

    init(editing: EnvStatus?) {
        self.editing = editing
        _name = State(initialValue: editing?.name ?? "")
        _type = State(initialValue: editing?.type ?? "link")
        _root = State(initialValue: editing?.root ?? "")
        _binDir = State(initialValue: editing?.binDir ?? "")
        _workDir = State(initialValue: editing?.workDir ?? "")
        _extraPath = State(initialValue: (editing?.extraPath ?? []).joined(separator: "\n"))
        _variables = State(initialValue: (editing?.env ?? [:]).sorted { $0.key < $1.key }
            .map { "\($0.key)=\($0.value)" }.joined(separator: "\n"))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text(editing == nil ? "新增執行環境" : "編輯執行環境").font(.title2.bold()).sheetHeading()
            Form {
                TextField("名稱", text: $name, prompt: Text("例如 vllm-metal；小寫、數字、-_.，最長 63 字")).disabled(editing != nil)
                Picker("類型", selection: $type) {
                    Text("連結既有環境").tag("link")
                    Text("一般目錄").tag("dir")
                }
                caption("連結：根目錄下有 bin（例如 Python venv、node 安裝）。一般目錄：任何工具鏈，bin 目錄可選。")
                directoryField("根目錄", "絕對路徑", $root)
                directoryField("Bin 目錄", "選填，預設為 根目錄/bin", $binDir)
                directoryField("工作目錄", "選填", $workDir)
                caption("額外 PATH：每行一個絕對路徑（選填，例如專案的 node_modules/.bin）")
                TextEditor(text: $extraPath).font(.system(.body, design: .monospaced)).frame(height: 44).border(.secondary.opacity(0.3))
                caption("環境變數：每行 KEY=VALUE（不可設定 PATH、VIRTUAL_ENV、MACCLUSTER_PORT）")
                TextEditor(text: $variables).font(.system(.body, design: .monospaced)).frame(height: 60).border(.secondary.opacity(0.3))
            }
            caption("只登記指向既有目錄，不會建立或修改它。設定儲存在受保護的本機狀態目錄；環境變數不建議放機密。只有根目錄含 pyvenv.cfg 時才會設定 VIRTUAL_ENV。")
            if !name.isEmpty {
                caption("將公布 Node label：env.maccluster.example.com/\(name)=true")
            }
            if let error { Text(error).foregroundStyle(.red).textSelection(.enabled) }
            HStack {
                Spacer(); Button("取消") { dismiss() }.keyboardShortcut(.cancelAction).disabled(model.busy)
                Button(model.busy ? "儲存中…" : "儲存") { Task { await save() } }
                    .glassPrimaryAction().disabled(model.busy || name.isEmpty || root.isEmpty)
            }
        }
        .padding(24).frame(width: 680).appScreen()
    }
    private func caption(_ text: String) -> some View {
        Text(text).font(.caption).foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, alignment: .leading).fixedSize(horizontal: false, vertical: true)
    }
    private func directoryField(_ title: String, _ prompt: String, _ value: Binding<String>) -> some View {
        HStack {
            TextField(title, text: value, prompt: Text(prompt))
            Button("選擇…") { pickDirectory(value.wrappedValue) { if let path = $0 { value.wrappedValue = path } } }
        }
    }
    private func save() async {
        var arguments = ["env", "add", "--name", name, "--type", type, "--root", root]
        if !binDir.isEmpty { arguments += ["--bin-dir", binDir] }
        if !workDir.isEmpty { arguments += ["--workdir", workDir] }
        for line in extraPath.components(separatedBy: "\n") where !line.trimmingCharacters(in: .whitespaces).isEmpty {
            arguments += ["--extra-path", line.trimmingCharacters(in: .whitespaces)]
        }
        for line in variables.components(separatedBy: "\n") where !line.isEmpty {
            guard let equal = line.firstIndex(of: "="), equal != line.startIndex else { error = "環境變數每行必須為 KEY=VALUE。"; return }
            arguments += ["--set-env", line]
        }
        model.busy = true; defer { model.busy = false }
        do {
            _ = try await AgentClient.command(arguments)
            await model.refresh()
            model.selected = .environment(name)
            dismiss()
        } catch { self.error = error.localizedDescription }
    }
}
