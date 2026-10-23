import SwiftUI

/// One view per help tab. Each page is a few cards, tables and code listings;
/// prose is kept to a line or two.

struct GeneralHelpPage: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HelpCard(title: "背景服務", systemImage: "gearshape.2") {
                FactRow(systemImage: "app.badge.checkmark", title: "放在固定位置", detail: "例如 Applications。移動 App 後要重新安裝 LaunchAgent。")
                FactRow(systemImage: "power", title: "開啟 App 時自動啟動", detail: "關閉視窗後仍持續運作；登入時也會啟動。")
                FactRow(systemImage: "person.fill", title: "以目前 macOS 帳號執行", detail: "\(NSUserName())。原生程序可執行任意指令並使用 Metal，namespace 不是主機隔離邊界。")
            }
            HelpCard(title: "狀態顏色", systemImage: "circle.lefthalf.filled") {
                VStack(alignment: .leading, spacing: 8) {
                    legend(.running, "Running／Ready", "正常運行，或已連線")
                    legend(.transitioning, "Pending／Joining／Restarting／Paused", "啟動、加入或重啟中")
                    legend(.stopped, "Stopped／Succeeded／Created", "已停止，或已正常結束")
                    legend(.failed, "Failed／Error／Dead／Removed", "異常，需要處理")
                    legend(.unknown, "Unknown", "無法確認（管理程式或 Docker 沒有回應，顯示的是上次的結果）")
                }
                Callout(kind: .info, text: "顏色一定搭配圓點與文字，不只靠顏色辨識。Compose 專案只有在每個容器都運行時才是綠色。")
            }
            HelpCard(title: "資料位置", systemImage: "folder") {
                HelpTable(rows: [
                    ["狀態資料", AgentClient.stateDirectory.path],
                    ["LaunchAgent", "~/Library/LaunchAgents/com.maccluster.agent.plist"],
                    ["設定入口", "設定 → 管理程式（停止／重新啟動）"]
                ])
            }
            HelpCard(title: "停止背景服務", systemImage: "stop.circle") {
                Text("停止或重新啟動管理程式時，它管理的容器與原生程序預設繼續執行，新的管理程式啟動後接手（像 kubelet 重啟）。").font(.callout)
                CodeBlock(title: "只停止管理程式（工作繼續執行）", language: .bash, code: "launchctl bootout gui/$(id -u)/com.maccluster.agent")
                CodeBlock(title: "停止所有工作再停止管理程式", language: .bash, code: "maccluster-agent drain --state-dir \"" + AgentClient.stateDirectory.path + "\"\nlaunchctl bootout gui/$(id -u)/com.maccluster.agent")
                Callout(kind: .warning, text: "管理程式不在時，沒有人重啟、探測或回報這些工作。解除安裝 App 前，請先離開叢集或停止所有工作，否則它們會繼續執行而沒人管理。")
            }
        }
    }
}

private func legend(_ tone: StatusTone, _ phases: String, _ meaning: String) -> some View {
    HStack(alignment: .firstTextBaseline, spacing: 10) {
        StatusBadge(text: tone.label, tone: tone).frame(width: 110, alignment: .leading)
        VStack(alignment: .leading, spacing: 2) {
            Text(phases).font(.system(.callout, design: .monospaced))
            Text(meaning).font(.callout).foregroundStyle(.secondary)
        }
    }
}

struct ContainersHelpPage: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HelpCard(title: "用哪個執行器？", systemImage: "shippingbox") {
                HelpTable(header: ["Pod template label", "結果"], rows: [
                    ["（沒有 executor label）", "Docker Desktop 容器（預設）"],
                    ["executor: docker", "Docker，明確指定"],
                    ["executor: native", "macOS 原生程序"],
                    ["其他值", "回報錯誤，Pod 維持 Pending"]
                ])
                Callout(kind: .info, text: "Docker Desktop 需要另外啟動。")
            }
            HelpExample(title: "Docker 範例（預設，不寫 executor）", file: "faq-docker.yaml", yaml: FAQExamples.docker, expanded: true)
            HelpExample(title: "Docker 卷宗範例", file: "faq-docker-volumes.yaml", yaml: FAQExamples.dockerVolumes,
                        note: "Docker 的 Pod 支援 emptyDir（含 medium: Memory 與大小限制）、configMap、secret、downwardAPI、projected（不含 ServiceAccount token）與白名單內的 hostPath；內容約每分鐘同步一次。容器重啟時內容保留；Pod 結束或被刪除後，管理程式會清掉它的卷宗。檔案權限位元在 Docker Desktop 內不是安全邊界。hostPath 預設全部拒絕，要在節點選項 allowedHostPaths 列出允許的目錄。")
            HelpCard(title: "本機服務與故障紀錄", systemImage: "list.bullet.clipboard") {
                FactRow(systemImage: "plus.circle", title: "左側 + 可建立本機服務", detail: "預設不自動重啟（Never），可改 OnFailure 或 Always。")
                FactRow(systemImage: "arrow.clockwise", title: "每 3 秒更新狀態", detail: "失敗次數與最後退出原因在自動重啟後仍保留。")
                FactRow(systemImage: "trash", title: "刪除本機服務是永久的", detail: "會移除定義與執行資源，不會自動恢復。")
            }
        }
    }
}

struct NativeHelpPage: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HelpCard(title: "欄位對應", systemImage: "terminal") {
                HelpTable(header: ["Pod 欄位", "原生程序"], rows: [
                    ["command", "要執行的程式（entrypoint），絕對路徑；選了環境可只寫指令名稱"],
                    ["args", "傳給程式的參數"],
                    ["image", "只是描述，不會下載任何東西"],
                    ["$(MACCLUSTER_PORT)", "agent 分配的本機埠，會在 args 與 env 展開"]
                ])
                Callout(kind: .info, text: "指令不會經過 shell。要用管線或變數時，明確寫 command: [/bin/sh, -c, \"…\"]。")
            }
            HelpExample(title: "Native：絕對路徑", file: "faq-native.yaml", yaml: FAQExamples.native, expanded: true)
            HelpExample(title: "Native：在執行環境內", file: "faq-native-environment.yaml", yaml: FAQExamples.nativeEnvironment,
                        note: "先在右上角「執行環境」新增名為 my-env 的環境，或把 YAML 裡的 my-env 換成你的環境名稱（label 與 nodeSelector 兩處都要改）。")
            HelpCard(title: "程式還沒裝在 Mac 上？", systemImage: "shippingbox.and.arrow.backward") {
                FactRow(systemImage: "checkmark.circle", title: "建議：登記成執行環境", detail: "Python venv、node 安裝都可以，改用 command: [python3] 這類指令。")
                FactRow(systemImage: "scroll", title: "或用腳本準備後再啟動", detail: "command: [/bin/zsh, -c]，在 args 寫可重複執行的安裝步驟。")
                FactRow(systemImage: "xmark.circle", tint: .red, title: "label 不能放安裝指令", detail: "目前也不會解析 annotation 裡的指令。")
            }
            Callout(kind: .warning, title: "Running 不等於可用", text: "安裝與啟動共用同一個程序，沒有獨立的「安裝中」狀態。請加 HTTP readiness probe 判斷是否就緒，失敗原因看服務日誌。")
        }
    }
}

struct ClusterHelpPage: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HelpCard(title: "叢集操作範圍", systemImage: "server.rack") {
                FactRow(systemImage: "eye", title: "只顯示指派給這台 Mac 的工作", detail: "不是叢集的全部工作。")
                FactRow(systemImage: "minus.circle", title: "刪除副本", detail: "控制器可能立即補回。")
                FactRow(systemImage: "trash", tint: .orange, title: "刪除所屬工作", detail: "會影響所有節點上的副本，需要管理 kubeconfig。")
                FactRow(systemImage: "rectangle.portrait.and.arrow.right", title: "離開叢集", detail: "只移除這台 Mac 的 Node 與工作，不影響其他叢集或本機服務。")
            }
            HelpCard(title: "管理副本", systemImage: "square.stack.3d.up") {
                CodeBlock(title: "調整、查看、刪除", language: .bash, code: """
                kubectl -n maccluster-test scale deployment/NAME --replicas=2
                kubectl -n maccluster-test get pods -o wide
                kubectl -n maccluster-test delete deployment NAME
                """)
                Callout(kind: .info, text: "透過 Deployment／ReplicaSet／Pod 管理 namespace 與 replicas；相容範圍逐項實測，不宣稱完整 Kubernetes 相容。")
            }
        }
    }
}

struct NetworkHelpPage: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HelpCard(title: "目前可以", systemImage: "checkmark.seal") {
                FactRow(systemImage: "checkmark.circle.fill", tint: .green, title: "本機 loopback 埠", detail: "服務在這台 Mac 的 127.0.0.1 上可連。")
                FactRow(systemImage: "checkmark.circle.fill", tint: .green, title: "固定埠（fixed-ports label）", detail: "程式綁 0.0.0.0 時，其他節點可用 Mac 的 LAN IP 加埠連線；沒有認證，也沒有 NetworkPolicy。")
            }
            HelpCard(title: "目前沒有", systemImage: "xmark.seal") {
                FactRow(systemImage: "xmark.circle.fill", tint: .red, title: "Pod IP、跨節點 Pod 網路")
                FactRow(systemImage: "xmark.circle.fill", tint: .red, title: "ClusterIP／Service、叢集 DNS")
                FactRow(systemImage: "xmark.circle.fill", tint: .red, title: "NetworkPolicy、Ingress")
                FactRow(systemImage: "exclamationmark.circle.fill", tint: .orange, title: "kubectl logs／exec／cp／attach／port-forward", detail: "需要啟用 kubelet 端點（opt-in）；目前只在測試節點驗證，現行節點未啟用時請在本機介面讀取日誌。")
            }
            Callout(kind: .info, title: "需要固定埠？", text: "用 fixed-ports label 宣告程式自行綁定的一個或多個埠，其他節點即可用這台 Mac 的 LAN IP 加該埠連線。範例見「套用 YAML」。")
        }
    }
}

struct YAMLHelpPage: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            HelpCard(title: "套用步驟", systemImage: "list.number") {
                CodeBlock(title: "把範例存成檔案後執行", language: .bash, code: """
                # 1. 確認本機 Node 名稱，替換 YAML 裡的 maccluster.example.com/host
                kubectl get nodes
                # 2. 建立 namespace（只需一次）
                kubectl create namespace maccluster-test
                # 3. 套用，並查看
                kubectl apply -f 檔名.yaml
                kubectl -n maccluster-test get pods -o wide
                # 4. 刪除
                kubectl delete -f 檔名.yaml
                """)
            }
            HelpCard(title: "每份 YAML 都需要", systemImage: "checklist") {
                HelpTable(header: ["欄位", "為什麼"], rows: [
                    ["managed: \"true\"", "原型保護；沒有它 Pod 會被拒絕"],
                    ["nodeSelector …/host", "只排到指定的 Mac"],
                    ["tolerations …/prototype", "節點有 NoSchedule taint；沒有容忍度不會被排程"],
                    ["automountServiceAccountToken: false", "建議設定：尚不支援掛載 token；沒設也能執行，但會有警告事件"],
                    ["每個 Pod 一個 container", "label 放在 spec.template.metadata.labels"]
                ])
            }
            HelpExample(title: "1. Docker（預設）", file: "faq-docker.yaml", yaml: FAQExamples.docker, expanded: true)
            HelpExample(title: "2. Native：絕對路徑", file: "faq-native.yaml", yaml: FAQExamples.native)
            HelpExample(title: "3. Native：執行環境", file: "faq-native-environment.yaml", yaml: FAQExamples.nativeEnvironment,
                        note: "先在右上角「執行環境」新增 my-env，或同時改 environment label 與 nodeSelector 的環境名稱。")
            HelpExample(title: "4. Native：固定埠（可多個）", file: "faq-native-fixed-ports.yaml", yaml: FAQExamples.nativeFixedPorts,
                        note: "程式自己綁定的埠用 fixed-ports label 宣告，多個埠以底線分隔。agent 不再分配埠，探測打 readinessProbe 指定的埠；同一台 Mac 只能有一個副本。")
            HelpCard(title: "Pod 停在 Pending", systemImage: "exclamationmark.triangle") {
                HelpTable(header: ["看到的訊息", "原因"], rows: [
                    ["沒有訊息、未指派節點", "nodeSelector／tolerations 不符，或沒有節點公布該環境 label"],
                    ["managed=true is required", "缺少 managed label"],
                    ["environment … is not defined", "這台 Mac 沒登記該環境，或目錄不見了"],
                    ["UnsupportedPodSpec", "用了尚未支援的功能（PVC 等卷宗、Native 的卷宗、init 容器、grpc 探針等）；訊息列出所有欄位，kubectl describe 的 Events 也有"],
                    ["HostPathNotAllowed", "hostPath 不在節點選項 allowedHostPaths 的白名單內（預設全部拒絕）；訊息列出目前的白名單"],
                    ["FailedMount", "卷宗的來源不存在（ConfigMap／Secret 或 hostPath）；來源出現後會自動啟動"],
                    ["CreateContainerConfigError", "env 引用的 Secret／ConfigMap 或其 key 不存在；建立後會自動啟動"],
                    ["fixed port N is already in use", "固定埠被占用（另一個工作或其他程式）；釋放後會自動啟動"],
                    ["Running 但 READY 0/1，connection refused", "程式沒聽 $(MACCLUSTER_PORT)，探測打到分配埠；固定埠請加 fixed-ports label"]
                ], monospacedFirstColumn: false)
            }
        }
    }
}
