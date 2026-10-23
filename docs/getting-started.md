# MacCluster 使用與建置指南

[返回專案首頁](../README.md)

MacCluster 是用來管理 **macOS 原生程序、Docker Desktop 容器與 Kubernetes 指派工作**的桌面應用程式，提供 SwiftUI GUI 與 Go CLI／背景 agent。

例如：同一台 Mac 可以讓 nginx 在 Docker 中運行，讓需要 Metal 的 vLLM 在 macOS 原生環境中運行，並由 Kubernetes Deployment 管理副本與生命週期。

本專案目前是持續驗證中的原型，使用 Virtual Kubelet 的 Node lifecycle controller，以及限定本節點的 Pod 協調器。**不宣稱完整 Kubernetes／kubelet 相容。** 網路與進階 Pod 功能須依各項文件及實測結果確認。

![MacCluster 主畫面：叢集、原生服務與 Docker Compose 容器](images/maccluster-overview.png)

> 畫面中的名稱與位址為截圖當時的環境；使用時請填入自己的設定。

## 主要功能

- 管理多個 Kubernetes 叢集，各自保存 profile、憑證與工作狀態。
- 預設以 Docker 執行工作，也可明確選擇 macOS 原生程序，使用 Metal 等本機資源。
- 顯示本機 Docker 容器，依 Docker Compose 標籤分組；支援啟停、日誌、容器設定與資源使用量查詢。
- 登記本機執行環境，例如 Python venv 或工具目錄，供原生工作選用。
- 檢視服務日誌、失敗資訊與 Kubernetes 運行事件。
- GUI 與 CLI 共用背景 agent；不開啟 GUI 也能操作。

## 建置需求

| 項目 | 說明 |
| --- | --- |
| macOS | App 最低部署目標為 macOS 14；Liquid Glass 效果需要 macOS 26。主要實測平台為 Apple Silicon。 |
| Xcode／Swift | 使用含 macOS 26 SDK 的 Xcode 26 工具鏈，並確認 `xcode-select` 指向該版本；專案使用 Swift 6。 |
| Go | 1.25 或更新版本，依 `go.mod` 為準。 |
| Docker Desktop | 執行 Docker 工作或查看容器時需要啟動；只有建置程式不必啟動 Docker。 |
| kubectl | 從命令列建立、查看或調整 Kubernetes 工作時需要。 |
| 網路 | 首次建置需要下載 Go／Swift 套件；加入叢集時需能連線到 control plane。 |

在專案根目錄確認工具鏈：

```bash
xcodebuild -version
xcrun --sdk macosx --show-sdk-version
swift --version
go version
```

### 建置 App 與 CLI

```bash
./scripts/build.sh
```

腳本會優先使用 `.tools/go/bin/go`，不存在時使用 `PATH` 中的 `go`。也可明確指定：

```bash
MACCLUSTER_GO="$(command -v go)" ./scripts/build.sh
```

完成後會產生：

```text
dist/
├── MacCluster.app       # GUI，內附背景 agent
└── maccluster-agent    # 可獨立使用的 CLI／背景 agent
```

開啟 App：

```bash
open dist/MacCluster.app
```

建置只使用 `./scripts/build.sh`，會依序編譯 agent、SwiftUI、組裝 App 並驗證簽章。每次正式建置都會清除 UI／Swift 快取再重編，只保留 Go 套件與編譯快取，不需要另一支 GUI 建置腳本。

```bash
# 檢查環境，並自動修復可處理的缺漏
./scripts/build.sh --check

# 關閉工具鏈自動選擇與下載（仍可正常編譯及下載專案依賴）
./scripts/build.sh --no-repair

./scripts/build.sh --help
```

正式建置會清除 `app/.build`，以及先前診斷使用的 `.local/swift-build`、`.local/sidebar-build`。Swift 的套件、模組與編譯快取集中在新的 `app/.build`，不重用共用套件快取；Go 的 `.tools/gomod`、`.tools/gocache`、`.tools/gopath` 與工具鏈保留。首次重新下載 Swift 套件及完整重編會比較久。`--check` 不清除 UI 快取。

預設自動尋找已安裝且相容的 Xcode，以這次程序的 `DEVELOPER_DIR` 選用，不修改系統設定。Go 缺少或太舊時，會從 `go.dev` 的官方發布清單選取相容穩定版，下載對應 Mac 架構的壓縮檔並核對 SHA256，再安裝至 `.tools/go`；舊的專案工具鏈若存在會另行備份。快取目錄自動建立，Go／Swift 缺少的套件由套件管理器在建置時取得。

首次下載需要網路；下載或 checksum 驗證失敗會停止，不會略過驗證。明確指定但無效的 `MACCLUSTER_GO` 會回報錯誤，請修正或取消該變數。若尚未安裝相容 Xcode、未完成首次啟動或 Apple 授權，需先由使用者完成；腳本不會自動接受授權、執行 sudo、重啟服務或修改叢集。

目前成品使用本機 ad-hoc 簽章，尚未提供 Developer ID 簽章與 notarization。首次執行下載的版本時，請依 macOS 顯示的提示與系統安全設定處理。

### 從 GitHub Actions 取得成品

專案的 `Build macOS app` workflow 會執行檢查、測試與建置。成功後可在該次執行的 Artifacts 下載 `MacCluster-macOS-arm64`，包含 App ZIP、獨立 agent 與 SHA256 檢查碼。此下載成品為 Apple Silicon 版本。

如有安裝 GitHub CLI，可在專案目錄執行：

```bash
gh run list --workflow build.yml --limit 5
gh run download RUN_ID --name MacCluster-macOS-arm64
shasum -a 256 -c SHA256SUMS.txt
unzip MacCluster-macOS-arm64.zip
```

請將 `RUN_ID` 換成實際執行編號。

## 第一次啟動與背景服務

一般啟動使用這個狀態目錄：

```text
~/Library/Application Support/MacCluster
```

App 會先連線既有 agent；無法連線時，會嘗試安裝並啟動目前使用者的 LaunchAgent。GUI 與 CLI 透過本機 Unix socket 溝通。背景服務需要該 macOS 使用者的登入工作階段。

- 關閉視窗或退出 GUI，不等於停止工作。
- 設定頁面的「管理程式」分頁提供背景服務操作。
- 一般 agent 重啟會嘗試接管既有工作；`service stop` 保留工作，`drain` 或 UI 的「停止管理程式與所有工作」才會停止工作。詳見[重啟設計](agent-restart-design.md)。
- LaunchAgent 記住執行檔的絕對路徑；搬移 App 後，需從新位置重新安裝背景服務。
- 不要對同一個狀態目錄啟動第二個 agent。

如果 macOS 詢問區域網路存取，請允許需要連線叢集的 App／執行工具。`no route to host` 不一定是 control plane 故障，也可能是執行工具的本機網路權限、路由或防火牆問題。

## GUI 使用方式

### 加入 Kubernetes 叢集

1. 在 control plane 取得有效的 `kubeadm join ...` 指令。
2. 點左上角 **＋ → 加入 Kubernetes 叢集**。
3. 填入叢集顯示名稱（選填），以及 Node 名稱。Node 名稱預設留白，右側「預設」會立即填入 `<整理後的主機名稱>-<隨機識別碼>`；也可留空，讓 Agent 在送出後自動產生。請確認沒有與既有節點重名。自訂名稱沒有固定前綴限制，只需符合 Kubernetes 的 DNS 名稱格式。
4. 貼上完整 join 指令。MacCluster 只解析其中參數，不會執行貼上的 shell 文字。
5. 視需要填入代理 URL，以及管理 kubeconfig 在 **這台 Mac 上**的絕對路徑。
6. 點「開始加入」，再確認側邊欄與詳細頁的連線狀態。

macOS 並不是直接執行 Linux 的 `kubeadm join`。平台會處理 CA 驗證、bootstrap CSR、Node 註冊與後續協調；若叢集未自動核准 CSR，需要叢集管理員處理。

**Bootstrap token 不代表管理員權限。** 管理 kubeconfig 用於刪除工作控制器、必要時移除 Node 等管理動作，實際能力由 Kubernetes RBAC 決定。它不是單純為了顯示 GUI 而要求的設定。

### 查看叢集與工作

- 左側選取叢集可查看連線資訊；只顯示指派給這台 Mac 的工作，不是整個叢集的所有 Pod。
- 點箭頭或雙擊叢集／Compose 群組，可以展開與收合。
- 選取工作可查看狀態、日誌與可用操作。外部 Docker 容器另外提供容器設定與資源使用量。
- Compose 分組依實際 Docker Compose 標籤辨識，不依容器名稱猜測。
- 「最後更新時間」表示目前畫面資料的更新時間；無法更新時不代表工作已停止。

### 建立本機服務

左上角 **＋** 可選擇新增 Docker 容器或原生終端服務。這些服務不屬於任何 Kubernetes namespace，也不需要先加入叢集。

- **Docker**：填入映像、埠與必要參數，Docker Desktop 必須可用。
- **原生程序**：指定可執行檔、參數、工作目錄，或選擇已登記的執行環境。程序以 agent 所屬的 macOS 使用者執行。
- 需要 shell 語法時，明確使用 `/bin/sh` 或 `/bin/zsh` 與 `-c` 參數；一般命令陣列不會自動解讀 shell 語法。

原生環境可從右上角「執行環境」管理入口新增、編輯、檢視與移除登記。登記不會自動下載程式、建立 venv 或安裝套件。

### 日誌與設定

- **服務日誌**：查看程序或容器輸出，支援 ANSI 顏色、換行與垂直捲動。
- **運行日誌**：記錄加入／退出叢集、節點移除、Pod 指派、狀態變化與移除等事件，INFO 為綠色、WARNING 為黃色、ERROR 為紅色。
- Kubernetes 運行事件保留最近 1,000 筆，退出叢集後仍保留；不會回補安裝前的歷史，也不是 Kubernetes audit log。
- 「設定 → 一般」可調整日誌字體等顯示選項；「管理程式」管理背景服務。
- 右上角 FAQ 提供 label、原生命令與 YAML 範例。

### 刪除與退出

本機服務可從詳細頁或右鍵選單刪除。外部 Docker 容器與 agent 管理的工作具有不同操作範圍，不會因為出現在清單就自動被接管。

叢集工作分為「刪除 Pod 副本」與「刪除所屬工作」：前者可能被 Deployment 自動補回；後者會刪除控制器，可能影響該控制器在其他節點上的副本。退出叢集會移除這台 Mac 的節點與該叢集的本機工作，不會刪除整個叢集。

## 純 CLI 使用方式

以下命令在專案根目錄執行。預設狀態目錄與 GUI 相同，不需加 `--state-dir`。

```bash
MC="$PWD/dist/maccluster-agent"
"$MC" --help
"$MC" env --help
"$MC" join --help
"$MC" status
```

Help 訊息為英文。`status` 等查詢主要輸出 JSON；有安裝 `jq` 時可自行加上 `| jq` 排版。

### 不啟動 GUI，只執行 agent

純 CLI 也能安裝及管理登入時啟動的背景服務：

```bash
# 首次安裝；已有 GUI 安裝的服務可略過
./dist/maccluster-agent service install
./dist/maccluster-agent service start
./dist/maccluster-agent service status
./dist/maccluster-agent status

# 保留工作，停止代理並避免 launchd 立即重啟
./dist/maccluster-agent service stop
./dist/maccluster-agent service start
./dist/maccluster-agent service restart
```

`service` 管理目前使用者的 `com.maccluster.agent`，與 GUI 共用。`install` 登記目前執行檔的絕對路徑，請放在固定位置；要更新已載入的服務設定，先 `service stop` 再 `service install`。`install`／`config` 使用 `--state-dir` 指定狀態目錄；其餘服務操作為相容舊版 UI 也接受此參數，但仍使用已登記的設定，不會切換服務的狀態目錄。`service status` 顯示 launchd 狀態；工作狀態仍用 `status` 查詢。

新版背景服務將代理放在獨立的輕量監督程序下：`kill -9 <maccluster-agent 的 PID>`（SIGKILL）後停止自動重啟，直到明確執行 `service start`、開啟 App 啟動服務或下次登入。一般 `kill <PID>`（SIGTERM）、正常退出及其他錯誤退出仍會自動重啟。此規則依收到的訊號判斷，不辨識發送者；請指定代理本身的 PID，不要用包含監督程序在內的整組 command-line 比對。前景 `serve` 沒有自動重啟機制。

從舊版升級時，已載入的 launchd 設定不會因檔案更新而改變。純 CLI 可執行以下命令套用新版設定（保留工作）：

```bash
./dist/maccluster-agent service stop
./dist/maccluster-agent service install
./dist/maccluster-agent service start
```

服務已卸載時，`service stop` 仍視為成功；`service restart` 也可以直接從已卸載狀態啟動，不需要先手動開啟 GUI。

`service stop` 保留 LaunchAgent 設定，下次登入仍會自動啟動；容器與原生程序繼續執行，但暫停監控及叢集回報。`drain` 會停止所有工作且 launchd 可能再次啟動代理，不等同於 `service stop`。

如果不需要登入時自動啟動，可在第一個終端執行：

```bash
./dist/maccluster-agent serve
```

在另一個終端執行後續管理命令。`serve` 是前景程序，不會自行安裝 LaunchAgent。已有 GUI 安裝的背景服務時，不要重複執行。

開發時可用獨立狀態目錄，但 GUI 與 CLI 必須指定相同路徑：

```bash
./dist/maccluster-agent serve --state-dir "$PWD/.local/agent"
# 在另一個終端執行：
./dist/maccluster-agent status --state-dir "$PWD/.local/agent"
open dist/MacCluster.app --args --state-dir "$PWD/.local/agent"
```

### 建立並登記 Python 環境

```bash
python3 -m venv "$HOME/.venvs/my-service"
# 視需求安裝套件，例如：
# "$HOME/.venvs/my-service/bin/pip" install PACKAGE_NAME

"$MC" env add --name my-service --root "$HOME/.venvs/my-service"
"$MC" env list
"$MC" env list --json
"$MC" env shell --name my-service
```

`env shell` 只輸出 shell 啟動設定 JSON，不會直接開啟互動終端。`env add` 可用同名登記覆寫設定，支援 `--bin-dir`、`--workdir`、重複的 `--extra-path` 與 `--set-env KEY=VALUE`。環境變更不會自動重啟既有工作。

### 加入叢集

`join` 從標準輸入讀取 JSON。以下為範例格式，需替換佔位值；可將實際 JSON 放入權限受限的檔案再輸入，避免把真實 token 留在 shell 歷史中。

```bash
"$MC" join <<'JSON'
{
  "name": "開發叢集",
  "nodeName": "maccluster-node",
  "command": "kubeadm join HOST:6443 --token TOKEN --discovery-token-ca-cert-hash sha256:HASH",
  "managementKubeconfig": "/absolute/path/admin.kubeconfig"
}
JSON
```

`nodeName` 可以省略或留空，由管理程式自動產生；自訂值無固定前綴限制，只須符合 Kubernetes 的小寫 DNS 名稱格式。不合法名稱會在接受加入要求前被拒絕。GUI 會隨後續狀態更新加入進度及錯誤。

`managementKubeconfig` 與 `proxyURL` 為選填。加入後，用 `status` 取得叢集 ID；後續命令的 `clusterID` 是平台 profile ID，不是叢集顯示名稱。

### 建立 Docker 與原生本機服務

Docker 範例（省略 executor 時也預設使用 Docker）：

```bash
"$MC" create <<'JSON'
{
  "name": "local-nginx",
  "executor": "docker",
  "image": "nginx:alpine",
  "containerPort": 80,
  "restartPolicy": "Always"
}
JSON
```

原生範例，使用前面登記的 Python 環境：

```bash
"$MC" create <<'JSON'
{
  "name": "local-http",
  "executor": "native",
  "environment": "my-service",
  "command": ["python"],
  "args": ["-u", "-m", "http.server", "$(MACCLUSTER_PORT)", "--bind", "127.0.0.1"],
  "restartPolicy": "Always"
}
JSON
```

回傳 JSON 包含工作 `uid` 與本機 `port`。`$(MACCLUSTER_PORT)` 由 agent 展開；上例使用帶引號的 heredoc，避免先被 shell 執行。需要固定埠的原生工作可使用 `fixedPorts`，見[固定埠說明](fixed-ports.md)。

### 查詢、日誌與 Docker 控制

```bash
"$MC" status
"$MC" logs --uid WORKLOAD_UID
"$MC" logs --cluster-id CLUSTER_ID --uid POD_UID
"$MC" inventory

printf '%s\n' '{"containerID":"CONTAINER_ID"}' | "$MC" docker-inspect
printf '%s\n' '{"containerID":"CONTAINER_ID"}' | "$MC" docker-stats
printf '%s\n' '{"containerID":"CONTAINER_ID"}' | "$MC" docker-logs
printf '%s\n' '{"containerID":"CONTAINER_ID","action":"start"}' | "$MC" docker-control
printf '%s\n' '{"containerID":"CONTAINER_ID","action":"stop"}' | "$MC" docker-control
```

請替換 ID；Docker ID 可由 `inventory` 取得。`docker-control` 僅操作外部 Docker 容器，平台管理的工作應透過服務管理或 Kubernetes 控制器操作。

### 刪除、環境移除與退出

以下命令會修改狀態，執行前請確認 ID：

```bash
# 刪除本機服務
printf '%s\n' '{"uid":"WORKLOAD_UID","clusterID":"","mode":"workload"}' | "$MC" delete

# 移除環境登記；不刪除 venv 目錄
"$MC" env remove --name my-service

# 更新某個叢集的管理 kubeconfig
printf '%s\n' '{"clusterID":"CLUSTER_ID","managementKubeconfig":"/absolute/path/admin.kubeconfig"}' | "$MC" configure

# 退出指定叢集，停止該叢集在此 Mac 上的工作
printf '%s\n' '{"clusterID":"CLUSTER_ID"}' | "$MC" leave
```

完整 JSON 欄位與行為見[管理 API 契約](management-api.md)。

## 從 Kubernetes 指派工作到 Mac

在 Deployment 的 **Pod template labels** 指定執行方式，而不是只放在 Deployment 自己的 metadata：

```yaml
spec:
  template:
    metadata:
      labels:
        maccluster.example.com/managed: "true"
        maccluster.example.com/executor: native
        maccluster.example.com/environment: my-service
    spec:
      nodeSelector:
        maccluster.example.com/host: maccluster-node
        env.maccluster.example.com/my-service: "true"
```

這是設定片段，不是完整 manifest。完整範例見：

- [Docker nginx](../examples/faq-docker.yaml)
- [examples 目錄](../examples)：`faq-*.yaml` 包含原生程序、環境與固定埠範例。

套用前先建立專用 namespace，並將範例的 `nodeSelector` 換成自己的節點名稱：

```bash
kubectl --kubeconfig /absolute/path/admin.kubeconfig create namespace maccluster-test
kubectl --kubeconfig /absolute/path/admin.kubeconfig apply -f examples/faq-docker.yaml
kubectl --kubeconfig /absolute/path/admin.kubeconfig -n maccluster-test get pods -o wide
```

- executor 省略或設為 `docker` 時使用 Docker；設為 `native` 時執行原生程序，其他值會回報錯誤。
- 原生環境與程式必須先存在這台 Mac；`image` 不會自動安裝 macOS 程式。執行檔與參數使用 `command`／`args`，不要把任意 shell 指令塞進 label。
- 節點有 prototype taint，範例包含對應 toleration。若另外有 `network-unavailable` taint，請先用 `kubectl describe pod` 確認 Pending 原因；只在工作不依賴尚未提供的 Pod 網路、且接受限制時，為該工作加入對應 toleration，不要直接移除節點標記。
- namespace 是 Kubernetes 的資源組織與授權範圍，不是 macOS 原生程序的安全隔離邊界。
- 固定埠通常限制同一台 Mac 只能有一個使用該埠的副本。Metal 記憶體尚無完整 admission 控制，不要直接增加大型模型副本數。

## 目前限制與進階功能

- 沒有完整 Pod overlay／CNI，Node Ready 不代表完整 Kubernetes 網路就緒。
- Service TCP 轉送、叢集存取與 kubelet 端點需要額外設定，並非加入叢集後自動具備；見[Service 網路](service-network.md)、[controller 部署計畫](service-controller-plan.md)、[kubectl 與 kubelet 功能](kubectl-and-kubelet.md)。
- Pod 規格、環境變數、安全設定、卷宗、探針與 lifecycle 各有支援範圍；不支援規格會回報原因，不能把 Linux Pod 的所有能力視為已支援。
- Native 接受任意指令，以 macOS 帳號權限運行；不提供容器等價隔離，資源限制也不等同 Linux cgroups。
- 多叢集 profile 獨立，但共用同一台 Mac 的 CPU、記憶體與埠；並非彼此隔離的主機。
- agent 與 control plane 斷線時，本機程序可能繼續運行，尚無完整跨節點 fencing。

詳細文件：

| 主題 | 文件 |
| --- | --- |
| 實測與清理紀錄 | [validation.md](validation.md) |
| Mac／Linux 節點差異與進度 | [node-comparison.md](node-comparison.md)、[node-gap-roadmap.md](node-gap-roadmap.md) |
| 環境變數與安全設定 | [env-and-security.md](env-and-security.md) |
| 卷宗 | [volumes.md](volumes.md) |
| 探針與生命週期 | [probes-and-lifecycle.md](probes-and-lifecycle.md) |
| agent 重啟與接管 | [agent-restart-design.md](agent-restart-design.md) |
| Docker registry 登入 | [docker-registry-login.md](docker-registry-login.md) |

這些文件包含不同階段的紀錄，請留意各節的日期、啟用條件及「已實測／尚待驗證」註記。

## 開發檢查與問題排查

```bash
# 使用已安裝的 Go 1.25+ 工具鏈
go test ./...
go vet ./...

# 不需要 GUI 與執行中 agent 的 Swift 測試
swift test --package-path app --disable-sandbox \
  --filter 'ANSILogTextTests|FAQExamplesTests|StatusDecodingTests'

# 提交前檢查文字中的機密與環境資訊
./scripts/check-privacy.sh
```

整合測試會建立資源，執行前請閱讀 `scripts/integration.py` 與 `scripts/management_integration.py` 的範圍，並使用專用 namespace／測試環境；不要直接對正式工作執行測試。

常見問題：

- **CLI 顯示 agent unavailable**：確認背景服務運行，CLI 與 GUI 使用相同 `--state-dir`，且由同一使用者操作。
- **Docker 工作無法啟動**：確認 Docker Desktop 已啟動，並檢查 `docker info` 與目前 Docker context。
- **Pod Pending**：用 `kubectl describe pod` 檢查 selector、taint、環境、埠占用或 UnsupportedPodSpec。
- **沒有玻璃效果**：確認 macOS 版本、系統輔助使用設定與建置 SDK。腳本會檢查 App 是否連結新版 SDK。
- **UI 排版問題**：`./scripts/dump-ui.sh [App 路徑]` 可產生視窗結構診斷；詳細機制見 `app/Sources/MacCluster/DevSnapshot.swift`。

圖示來源為 `app/Resources/AppIcon.svg`。修改後可執行 `./scripts/build.sh` 自動更新 icns，並一併提交 `AppIcon.icns` 與 `AppIcon.sha256`；本機重建圖示需要圖形工作階段，CI 不會代為產生。

`.local/`、`.tools/`、`dist/` 不納入版本控制。請勿提交真實 join token、私鑰或完整 kubeconfig；截圖也應在公開前確認是否包含不想公開的資訊。


### App 的 Agent 連線與程序

App 直接透過 `agent.sock` 的 HTTP API 查詢狀態與送出 Agent 操作，不會為了查詢另外執行 `maccluster-agent status`，socket 不存在或拒絕連線時也不會退回 CLI 或啟動 Agent。停止後主畫面顯示無法連線與資料過期的提示；只有 App 初次開啟的既有自動啟動流程，以及明確按啟動／重新啟動，才會要求啟動服務。

Docker 清單、Docker 操作、本機環境登記及 launchd 管理使用 App 隨附的 `maccluster-tools`。它只打包在 `MacCluster.app/Contents/Resources`，不另放在 `dist` 頂層；這是短暫的本機命令工具，不接受 Agent API 查詢或 `serve`，不會變成另一個 Agent。啟動設定仍明確指向 `maccluster-agent serve`，同一狀態目錄的單一 Agent 由檔案鎖保護。原本供使用者手動操作的 `maccluster-agent` CLI 維持相容；手動執行其查詢命令仍會建立一次性的 CLI 程序。
