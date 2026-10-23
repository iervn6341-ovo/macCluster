# 實作驗證紀錄

> 本文件中的主機位址、使用者名稱、節點名稱與模型路徑已改為示範值（`192.0.2.0/24`、`198.51.100.0/24` 為文件保留位址，節點名稱為 `maccluster-node`，路徑為 `~` 或 `/path/to/...`）。實際環境不同；文中「80 叢集」「204 叢集」是原叢集位址末段的代稱。

測試日期：2026-10-15，Asia/Taipei。以下區分實測與尚未完成項目。

## 環境

- macOS 26.6.1、arm64、48 GiB RAM。
- Swift 6.4；專案內 Go 1.27.2；Docker Desktop Engine 29.8.2。
- API Server `192.0.2.10:6443` 回報 Kubernetes v1.28.15；既有 CNI 為 Flannel。
- 專用 Node：`maccluster-node`。專用 namespace：`maccluster-test`。
- vLLM 環境：`~/.venv-vllm-metal`，Python 3.12.13、vLLM 0.28.0+cpu、vllm-metal 0.28.0.dev20260909202646、MLX 0.32.1。
- 模型：`/path/to/models/gemma-4-E4B-it`。

## 已實測

1. CA SPKI hash 驗證、bootstrap CSR、自動核准及節點憑證取得成功。Agent 使用節點憑證註冊，沒有把管理員 kubeconfig 用作 Agent 身分。
2. `kubectl get nodes` 可看到專用 Node Ready，持續回報 lease；Node 有原型專用 taint。
3. nginx 的未指定 executor Pod 預設建立 Docker 容器。
4. 帶 `executor: native` 的 HTTP Pod 建立 macOS Python 程序。
5. 兩種 Deployment 的 1 → 2 → 1 → 0 → 1 副本測試通過；檢查 Kubernetes Ready、真實執行器數量、各副本獨立埠與 HTTP 200。
6. 正常停止／重新啟動 Agent 後，以已保存節點憑證重新連線並恢復 desired state。
7. Metal 實測為 `Device(gpu, 0)`。vLLM 工作日誌顯示 Metal platform、MLX GPU、PyTorch MPS 與 Metal paged-attention kernels。
8. vLLM Deployment 成為 Ready，`/health`、`/v1/models`、`/v1/completions`、`/v1/chat/completions` 均有實際成功回應。
9. 聊天測試輸入：`Reply in one short sentence: what is Kubernetes?`。回覆：`Kubernetes is an open-source system for automating the deployment, scaling, and management of containerized applications.` 共 19 prompt tokens、23 completion tokens。這是基本推論 smoke test，不是模型品質或效能評測。
10. SwiftUI release build 與 `.app` 組裝成功，實際開啟 App，確認真實 Node／工作列表及 vLLM 日誌可讀。
11. `go test -race ./...` 通過。包含 executor 生命週期／重啟／清理、Docker owner 恢復與外部容器排除、pull stderr 解析、bootstrap 格式與 CA pin、readiness 埠與門檻、不同安裝的 Node 名稱衝突拒絕。

## 測試中修正的問題

- Kubernetes 預設填入空的 Pod securityContext，現在允許空值而拒絕尚未支援的非空設定。
- Docker 首次拉取映像的 stderr 曾混入 container ID；成功結果現在只解析 stdout。
- Docker Desktop 的壓縮日誌設定與 max-file=1 衝突；改為兩個有限大小檔案。
- Pod 刪除先停止其 UID 對應工作，再用 UID precondition 完成已進入刪除狀態的 Pod 清理。
- 重啟 backoff 期間不把 Always 工作標成不可再恢復的 terminal Pod。
- Node 安裝 ID、獨立 shutdown/cleanup timeout、readiness 目標埠與門檻已補強。
- 現有 vLLM 環境同時有 mlx／metal 外掛；工作專用 `VLLM_PLUGINS=metal` 解決衝突，未移除套件。

## 操作狀態與檔案

- 本次使用 `.local/agent` 作為開發 Agent 狀態目錄，UI 以 `--state-dir` 指向它；這與一般 App 預設的 Application Support 目錄不同。
- `.local/admin.kubeconfig` 僅供這次授權的叢集操作；節點憑證另存 `.local/agent/node.kubeconfig`。這些檔案權限 0600，且整個 `.local` 已被 `.gitignore` 排除。
- 沒有保存 join token 或 SSH 密碼到專案檔案。
- vLLM 測試完成後縮到 0 釋放 GPU 記憶體。可重新 apply `examples/vllm.yaml` 啟動單副本。
- integration script 預設保留 nginx／native-http 各一副本；最終交付訊息另記錄實際保留狀態。
- 本次沒有安裝使用者 LaunchAgent；GUI 已提供安裝與停止入口，但尚未驗證登入重啟流程。

## 尚未驗證／未完成

- Go／kubectl 在本機直接連接 control plane 時回報 no route to host；curl 與 SSH 可達。實測透過 `127.0.0.1:18080` 的臨時 SSH SOCKS 隧道成功；尚未找出直連失敗根因。隧道中斷會使 Agent 無法同步叢集。
- 沒有跨節點 Pod 網路、ClusterIP、叢集 DNS、NetworkPolicy 或標準 kubectl logs／exec／port-forward；本次結果僅證明排程控制與本機執行。
- 尚無自動憑證輪替、Native 崩潰接管、Metal 容量 admission、完整 Pod termination 與 volume／security 語意。
- 網路、原生程序與資源隔離尚不具 Kubernetes 全相容性；不得將目前 Node Ready 解讀為已完成這些能力。
- App 尚未 Developer ID 簽章或 notarization。

## 2026-10-15 管理介面與生命週期增量驗證

- SwiftUI 側邊欄實際操作：展開既有叢集顯示 nginx/native-http，`+` 顯示加入叢集／Native／Docker 三入口；由表單建立 `/usr/bin/false` 測試服務，畫面顯示 Failed、失敗 1、NonZeroExit 與 exit code 1。刪除確認範圍文字已檢視，測試服務由 API 清理。
- `scripts/management_integration.py` 於真實 Docker Desktop／macOS 通過：SIGKILL 自建 sleep 程序，Always 自動重建並保留歷史；外部 `docker rm -f` 自建 nginx，Never 顯示 Failed/ContainerRemoved；所有本機測試服務已清理。
- 使用既有 control plane 和管理 kubeconfig，在 `maccluster-test` 建立獨立 `management-delete-check` Deployment：API 刪除單 Pod 後看到不同 UID 的替代副本；API 刪除所屬工作後 Deployment 消失。測試資源已清理，原 nginx/native-http 保留。
- 多叢集分組／刪除隔離，以及退出成功／遠端拒絕後保留重試設定，使用 fake runtime 與本機 HTTP API 測試。沒有退出現用叢集，也沒有宣稱已與第二個真實 control plane 同時連線。
- 完整 Go race 測試、go vet 與 Swift release 建置通過。打包改用新 inode 取代 executable 並重新 ad-hoc 簽章，修正重建後 macOS 拒絕載入 App 的問題；非 Developer ID 發行簽章。
- 異常記錄目前保存每 UID 的最後事件與失敗數；不是完整歷史稽核或通知中心推播。vLLM 本次未重新載入模型；既有 vllm-metal 維持零副本。

## 2026-10-15 第二個真實叢集回歸（保留待確認）

- 新 control plane `192.0.2.20:6443` 已驗證 CA pin、完成 bootstrap，profile `2e98aedb26dcbd4f590f857886dbdd16`（測試叢集 204），Node `maccluster-node`。與原 `192.0.2.10` 同時 Ready。
- 本輪一開始恢復既有 SSH SOCKS 通道（127.0.0.1:18080）；兩個 profile 均依賴此暫時通道，通道停止會失去控制連線。
- 使用者要求不動既有 namespace：在新叢集獨占建立 `maccluster-e2e-20261015-204`，僅於其中建立 nginx / native-http；兩種 executor 的 replicas 1→2→1→0→1、Ready、獨立埠與 HTTP 200 全部通過。
- 使用者提出此限制前，已啟動原叢集既有 maccluster-test 的回歸測試；已完成且恢復 nginx/native-http 各一副本，之後未再寫入其 namespace。
- 新 namespace 的 nginx 執行 docker kill、Native HTTP 執行 SIGKILL，兩者自動恢復，failureCount/restarts 均為 1，HTTP 200；原叢集程序 PID、容器 ID 與 Pod UID 在此故障測試前後完全一致。
- 新叢集其他 namespace 的 Deployment/StatefulSet/DaemonSet/Pod 共 31 項，測試前後 UID 與 spec 比對一致。快照位於忽略版本控制的 .local 目錄。
- 完整 Go race suite 通過（快取）；本機 Docker 外部移除與 Native SIGKILL 自動恢復測試通過並清理當次本機測試服務。
- 透過 Computer Use 展開兩個叢集、選取兩種服務、查看 namespace/日誌/退出碼與重啟紀錄。工具禁止操作 Terminal App，命令執行使用開發工具，GUI 操作使用 Computer Use。
- 修正無參數開啟 App 時與已安裝 Agent 使用不同資料目錄：同一 executable 的 LaunchAgent 所指定 state-dir 作為預設；顯式 --state-dir 優先。Swift release 重建通過。
- 新 namespace、兩個 Deployment、Node 與 profile 保留，等待使用者確認後才能刪除。此輪未做退出／刪除 controller 的破壞性重測；其回歸測試由 Go suite 覆蓋。本次原生測試為 Python HTTP，不是 vLLM 模型載入。

## 外部 Docker 清單

新增唯讀 inventory CLI 與 SwiftUI 顯示，真實 Docker Desktop 探索出 5 個外部容器（Running 2、Stopped 2、Created 1），排除兩個 MacCluster 叢集 nginx。Swift release 建置與 Computer Use 視窗驗證通過：本機清單可選取 open-notebook，顯示映像與公開埠，無刪除入口。僅重開 GUI，未重啟 daemon 或既有容器／vLLM。

## 外部 Docker 手動啟停

新增 CLI docker-control 與 UI 啟動／停止／開啟 Docker Desktop。操作前以完整 ID inspect 驗證歸屬，真實叢集容器 stop 請求已確認遭拒且未執行；獨立測試容器經 Computer Use 操作 Created→Running→Stopped 通過，完成後僅清理該測試容器。原本機 vLLM PID 38825 不變。Swift release build、go vet 通過。開啟 Docker Desktop 按鈕已發送系統開啟要求；Computer Use 讀取 Docker Desktop 視窗逾時，該視窗未完成視覺驗證。

## 連線與本機執行狀態分離

確認兩叢集 Error 原因為 127.0.0.1:18080 SOCKS listener 停止；以 ssh -f -N 與 keepalive 恢復背景通道後，兩 profile 回到 Ready。通道仍不是自動重連或登入服務，主機重開／SSH 中斷後仍需恢復。SwiftUI 將叢集連線狀態和本機 executor 狀態分離，只有本機查詢失敗才標示上次狀態；內容頁頂端顯示選取叢集名稱／本機服務，不再顯示管理程式正常。Swift release build 與 Computer Use 叢集標題驗證通過，未重啟 Agent 或模型。

## 日誌捲動介面

服務日誌改用 NSScrollView / NSTextView，固定顯示雙向捲軸、保留長行、支援選取複製，並增加頂端／底部按鈕；同份日誌更新時保留閱讀位置。Swift release build 通過。Computer Use 使用本機 vLLM 日誌實測垂直捲動、向右／向左捲動，並確認底部按鈕將垂直位置設為 1、頂端回到 0；截圖確認雙向捲軸可見。僅重開 GUI，未重啟背景 Agent 或 vLLM。

## 2026-10-15 使用者確認清理 204 測試 namespace

依使用者明確指示刪除 `maccluster-e2e-20261015-204`，kubectl 等待刪除完成後查詢確認不存在。Agent 該 namespace 工作數為 0，nginx Docker 容器已移除、native-http PID 已退出。前後比對原 80 叢集的工作 UID／PID／容器 ID／phase 與本機服務 UID／PID／phase 均未變。204 叢集 profile 與 Node 保留。

## 2026-10-15 遠端移除 Node 偵測

- Agent 保存 Node UID，每輪約 3 秒先驗證 Node，再同步 Pod。API 明確 NotFound 或同名不同 UID 才判定 Removed；連線、403、500 錯誤不觸發清理。VK 啟動期間也禁止隱式重新建立 Node。
- Removed 時保存時間／原因，停止該 profile 的 executor 工作；重啟時不重新註冊。清理失敗會保留 Removed 並顯示錯誤，重啟或手動退出可重試；不宣稱強制停止一定成功。
- 相容既有 Leaving profile：啟動時若驗證遠端 Node 已消失，轉為 Removed。真實 204 在更新前已是退出失敗（node credential 無 delete Node 權限），遠端 Node 已不存在；新版辨識為 Removed，Computer Use 確認側欄與運行日誌顯示移除原因與時間。
- `go test -race ./...` 與 Swift release build 通過，測試包含 404、UID 變更、403／500、連線失敗、清理範圍、清理失敗與 tombstone 重啟不註冊。
- 真實 204 另建獨立 Node／namespace `maccluster-removal-check-20261015`，執行 Docker nginx 與 Native HTTP。刪除 Node 後兩工作自動清除，確認容器不存在、原生 PID 不存在；獨立 Agent 重啟仍 Removed、遠端 Node 未重建。測試 namespace 刪除初次等待 30 秒逾時，後續確認已由叢集清乾淨，兩測試 Pod 均 NotFound。未修改其他 namespace。
- 本次套用背景程式更新曾正常重啟 Agent 兩次，80 叢集恢復 Ready 且兩工作 Running。vLLM 因更新而重載，目前 /health 逾時，程序取樣卡在檔案 open；模型位於 NFS `198.51.100.1`，TCP 2049 探測回報 no route to host，但三份小型模型設定檔仍可讀（可能已有快取）。尚未證明模型已恢復可用，未擅自變更掛載或網路。

## 2026-10-15 ANSI 日誌色彩

SwiftUI 日誌以 attributed text 顯示 ANSI SGR：基本／亮色、256 色、RGB 前景與背景、粗體、底線及 reset；CSI 非樣式指令與 OSC 不執行，複製文字不包含 escape 色碼。此元件是日誌顯示器，不模擬終端游標或覆寫進度條；服務需自行輸出 ANSI 色碼。

Swift release build 與 3 項 XCTest 通過（色彩／重設、擴充色與背景、Unicode／控制碼／截斷輸入）。Computer Use 使用臨時本機 ansi-log-preview 程序確認實際顏色、背景、粗體與底線；水平及垂直捲動正常，重讀同一份日誌前後捲軸位置一致。測試程序已清除，只重開 GUI，沒有重啟 Agent。

## 2026-10-15 日誌自動換行

依使用者更新偏好，日誌取消水平捲軸，NSTextView 隨視窗寬度自動換行，只保留垂直捲動。ANSI attributed text、複製及頂端／底部功能保留。Swift release build 通過；Computer Use 確認 vLLM 長行自動折行、只有上下捲動控制項，底部按鈕可將垂直位置移至 1。僅重開 GUI。

## 2026-10-15 日誌字體與間距偏好

設定頁新增字體大小 10–32 pt、字元間距 0–5 pt、額外行距 0–16 pt；提供共用日誌元件的 ANSI 預覽與還原預設值。AppStorage 自動保存並觸發服務日誌即時重新排版，保留自動換行。Swift release build 通過；Computer Use 調整為 13 pt／0.5 pt／1 pt，確認預覽並重開 App 驗證三值保留，最後還原為 12／0／0 預設值。僅重開 GUI，未重啟背景服務。

## 2026-10-15 日誌交替底色

以 NSTextView 背景繪製逐筆日誌深淺交替底色，依實際換行字元分組，自動折行的片段維持同一底色。保留 ANSI 屬性與既有字體／間距設定；底色不寫入可複製文字。Swift release build 通過，Computer Use 截圖確認 vLLM 日誌交替底色及長行同色換行。僅重開 GUI。

## 2026-10-15 初始空白頁對齊

修正未選取服務時 detail VStack 未佔滿可用高度，導致標題與更新時間垂直置中的問題。內容容器明確填滿並靠左上對齊，空白提示獨立佔據剩餘區域置中。Swift release build 通過；Computer Use 重開 App 截圖確認初始標題／時間固定於頂端，提示位於下方內容中央。未重啟 Agent。

## 2026-10-15 側欄點擊切換

叢集標籤與服務列擴展為整列點擊範圍。服務原本獨立的 simultaneous acknowledgement 手勢改成在同一次點擊中明確設定選取，再確認通知；叢集標籤也明確選取，展開箭頭保留獨立操作。Swift release build 通過，Computer Use 單擊外部 Docker、叢集標籤及展開後 nginx 皆立即顯示對應資訊。未重啟背景服務；未直接驗證使用者的遠端桌面傳輸延遲／視窗啟用行為。

## 2026-10-16 Compose 資訊與日誌初始位置

- Docker inventory 透過 Docker ps 的 Label formatter 分別取得 Compose project/service/working_dir/config_files，避免平面 Labels 字串把多設定檔逗號拆壞。真實 inventory 驗證 4 個 Compose 容器與 1 個無 Compose 標籤容器；UI 顯示 open-notebook 專案、服務、工作目錄及檔案路徑。資訊僅代表建立時標籤，不讀取或執行 Compose 設定。
- NativeLogView 進入服務／讀取日誌完成後跳到底部，載入中佔位文字不消耗首次定位；一般重新排版仍保留閱讀位置。Computer Use 確認開啟 vLLM 日誌垂直捲軸為 1。
- Docker Desktop 4.94.0 嘗試指定容器外部連結：`dashboard/containers/<id>/info` 僅到容器清單；`dashboard/apps/<id>` 被當成 Compose 專案且錯頁。兩者均不保留，最終保留開啟 Docker Desktop 並新增複製容器 ID，未宣稱可直接跳轉單一容器。Docker Desktop CUA 初次選取耗時異常（約 16 分鐘），後續觀察正常。
- runtime Go tests、Swift release build 通過；只重開 GUI，未重啟 Agent／容器。

## 2026-10-16 Docker Inspect

Docker 容器詳細頁新增 Inspect 彈窗（本機與叢集容器皆可，原生程序不顯示），以完整 64 位十六進位 ID 執行唯讀 `docker inspect --type container`。顯示格式化 JSON、重新讀取及複製，使用現有換行／字體／底色元件但預設從 JSON 頂端開始。真實 open-notebook 容器驗證回傳 ID 一致，不合法 ID 被拒絕；Swift release build 與 Computer Use 彈窗載入驗證通過。未把 inspect 完整內容寫入文件或工具輸出，未重啟背景服務或容器。

## 2026-10-16 Docker Stats

Docker 容器詳細頁新增 Stats 彈窗，唯讀取樣指定完整 ID，顯示 CPU、記憶體用量／上限與比例、網路及磁碟累計 I/O、PIDs。每次取樣完成後約 3 秒更新，可取消自動更新或手動重新讀取；關閉彈窗停止後續輪詢，當次命令最多沿用 Docker helper 30 秒 timeout。停止容器回報沒有即時用量，錯誤時不把舊數值當目前資料。

Swift release build 通過。真實 surrealdb 執行中容器與 open-notebook 停止容器回傳驗證通過；Computer Use 確認 surrealdb Stats 彈窗顯示各項數值及成功更新時間。僅重開 GUI，未重啟背景服務或容器。

## 2026-10-16 Compose 側欄結構

本機清單依 Compose project 標籤分組為可展開的專案 → 容器，獨立容器與本機原生服務維持單列。專案列顯示容器總數／Running 數，點選專案可查看服務及狀態總覽，子列保留既有容器詳細資訊、資源使用狀況與啟停操作。Swift release build 通過；Computer Use 確認 open-notebook 2 個容器（1 Running）、另兩個 Compose 專案各 1 個，展開並單擊 surrealdb 能切換資訊。只重開 GUI。

### 2026-10-16 主視窗關閉後重新開啟

- 主視窗改為固定 id 的 SwiftUI Window；AppDelegate 在 reopen 事件以 openWindow 恢復主視窗，並明確禁止最後視窗關閉時退出 App。
- `scripts/build-app.sh` 編譯與簽章驗證通過。
- Computer Use 實際按紅色關閉鈕，確認 App 仍在執行，再透過系統開啟同一 App，主視窗成功恢復且叢集／Compose 列表正常顯示。
- 僅重新載入 GUI，未重啟背景 agent 或操作既有服務。

### 2026-10-16 外部本地 Docker 容器日誌

- 移除外部容器日誌的固定提示，新增 `docker-logs` CLI，以完整容器 ID 呼叫 `docker logs --tail 500`；沿用逾時、輸出大小限制與 stdout/stderr 合併。
- UI 選取外部容器時讀取實際日誌，沿用 ANSI、換行與初始捲至底部；讀取日誌按鈕可更新快照。
- `scripts/build.sh` 通過。唯讀比對執行中的 surrealdb 與停止的 open-notebook，輸出均與 Docker CLI 相符；非法 ID 被拒絕。
- Computer Use 點選本地 Ollama，已顯示實際日誌，捲軸位置為底部；僅重啟 GUI，未重啟背景 agent 或容器。

### 2026-10-16 Liquid Glass 操作介面

- macOS 26.6.1、Swift 6.4 SDK 編譯原生 `.glass` 按鈕，以 `GlassEffectContainer` 分組；套用於容器啟停、詳細資訊、資源使用狀況與日誌操作。
- 側邊欄明確採用系統 sidebar 樣式，工具列維持原生元件；日誌與資訊區不加玻璃背景，避免文字對比降低。
- macOS 26 以下或啟用減少透明度時，操作按鈕退回 bordered。舊 OS 與輔助使用設定分支未實機切換測試。
- `scripts/build-app.sh` 編譯與簽章驗證通過；Computer Use 載入新版並選取 Ollama，截圖確認操作區與日誌正常排版。未操作容器啟停或重啟 agent。
- API 依據：https://developer.apple.com/documentation/SwiftUI/Applying-Liquid-Glass-to-custom-views

### 2026-10-16 全畫面樣式與設定分頁

- 主內容採用共用低彩度背景與原生 glassEffect 標題面板；加入／設定叢集、新增 Docker／原生服務、運行日誌、Inspect、Stats、FAQ 與設定套用共用背景和原生玻璃按鈕。側邊欄與工具列保留系統樣式，文字日誌與 JSON 保留原閱讀背景。
- 設定改為一般／管理程式兩分頁，原有 AppStorage 值和危險操作確認流程保留。
- 編譯與簽章檢查通過；Computer Use 驗證兩個設定分頁切換、原有行距 7 pt 保留、運行日誌視窗顯示。未觸發管理程式或容器啟停。
- 檢查時修正運行日誌空狀態標題垂直位置，使內容靠上排列。未逐一實測全部表單提交（本次僅樣式變更）。

### 2026-10-16 補齊側邊欄、工具列與設定玻璃面板

- 側邊欄隱藏 List 原背景，增加內縮原生 glassEffect 面板；右上圖示整合至同一玻璃工具列，保留各項操作與輔助使用名稱。
- 一般／管理程式設定內容各有玻璃面板，設定視窗調整至 650×550。
- 實際截圖發現設定預覽的 NSTextView 文字未繪出，改以共用 ANSI 解析器產生 SwiftUI 彩色文字預覽，確認文字恢復；服務日誌元件未改動。
- 編譯、簽章檢查通過，Computer Use 確認三處面板、設定開啟與兩分頁切換。僅重啟 GUI。

### 2026-10-16 全 UI 樣式走查與遺漏修正

- 補上左上新增／側邊欄切換、設定分頁切換、叢集操作、Compose 卡片、加入／建立主要按鈕，以及 Join、Configure、Create、Inspect、Stats、運行日誌、FAQ 的玻璃標題區。
- 設定切換改成共用玻璃面板內的兩個具選取狀態按鈕；一般／管理程式分類與設定值保持不變。修正 FAQ 的設定路徑文字。
- 共用畫面按鈕也遵守減少透明度設定。保留輸入欄位、日誌、JSON 的可讀底色，系統選單及確認 alert 使用 macOS 原生呈現。
- 實際開啟檢查：一般／管理程式設定、Join、Native Create、Docker Create、叢集內容／Configure、Compose、外部容器、Inspect（避免輸出完整 JSON）、Stats（成功讀取數值）、FAQ、運行日誌、原生服務頁。
- 最終版本實測側邊欄隱藏與恢復，只有一顆切換按鈕。所有表單均取消，未執行建立、刪除、離開、服務啟停或管理程式重啟。確認 alert 沒有觸發實測。
- `scripts/build-app.sh` 編譯及簽章檢查通過，已載入 dist/MacCluster.app。

### 2026-10-16 互動玻璃與圖示尺寸

- 原有面板使用靜態 regular glass；新增 InteractiveGlass，工具列、側邊欄切換、新增選單與設定分頁使用 regular.interactive()。
- 工具列圖示 22 pt、點擊區至少 44×38 pt；側邊欄 Label 圖示 20 pt。設定分頁按下縮放與彈簧回復，選取背景使用 matchedGeometryEffect 平滑切換。
- 減少動態效果時不使用自訂縮放／切換動畫；減少透明度保留不透明替代背景。
- 編譯與簽章通過，CUA 截圖確認圖示放大及設定排版，實際切換兩分頁成功。靜態截圖無法驗證遠端畫面傳輸時的動畫流暢度，未宣稱該項已驗證。未變更服務。

### 2026-10-16 緊湊框線、純文字分頁與無色選取

- 工具列圖示點擊框由 44×38 改成 34×30 pt，縮小面板圓角與標題／設定內距。
- 設定分頁改成 13 pt 純文字，移除圖示與藍色填色，以 clear glass 和細邊線標示選取；保留切換動畫。
- 側邊欄改以模型選取狀態繪製 clear glass，移除原生 List 藍色選取底。服務列使用原生 Button，仍執行選取、日誌讀取與異常已讀處理；叢集／Compose 同樣顯示無色選取背景。
- 編譯與簽章通過；CUA 點選 Ollama 確認內容／日誌載入、側邊玻璃選取顯示，並確認設定純文字與兩分頁切換。未更動服務。

### 2026-10-16 FAQ 分類分頁

- 說明拆為一般與管理、容器與日誌、原生執行、叢集與副本、網路限制五頁，既有說明完整保留。
- 使用無間隙純文字分段列、中性選取底板與清楚邊框；標題和分類固定在捲動區外，切換分類回到頂端。
- 編譯、簽章通過；CUA 檢查外觀並逐一切換五頁，確認對應內容存在。僅重載 GUI，未重啟服務。

## 2026-10-16 原生執行環境階段 1 實測

- 單元測試：`go vet ./...`、`go test -race ./...` 通過（envs 登記／查找、runtime 環境啟動與拒絕案例、cluster label 解析與 Node label 同步）。
- 實測前狀態：執行中的 vLLM 是本機服務 `vllm-gemma-local`（`/health` 200），不是叢集 Deployment；叢集 `vllm-metal` Deployment 為 0/0 副本。80 叢集 `maccluster-test` 僅有該 Deployment。NFS 2049 可連、模型目錄可讀。
- 備份：`.local/backup-env-phase1/`（Deployment YAML、profiles、本機服務定義、舊版 agent）。
- 以 `scripts/build.sh` 重建，登記環境 `vllm-metal`（連結 `~/.venv-vllm-metal`，`VLLM_PLUGINS=metal`），`launchctl kickstart -k` 重啟 LaunchAgent。80 叢集回到 Ready。
- Node `maccluster-node` 約數秒內出現 `env.maccluster.example.com/vllm-metal=true`，確認節點憑證可 patch 自己的 Node label。
- 臨時 Deployment `env-check`（label `environment: vllm-metal`、nodeSelector 使用上述 label、`command: [python3, ...]` 裸名稱）：Pod Ready，日誌顯示 `sys.executable` 為 venv 內 python、`VIRTUAL_ENV` 與 `VLLM_PLUGINS=metal` 已套用，HTTP 200。
- 臨時 Deployment `env-check-missing`（environment 不存在）：Pod 維持 Pending，訊息 `environment "does-not-exist" is not defined on this node`。
- 兩個臨時 Deployment 已刪除，確認 Pod 與原生程序皆清除。
- **未驗證**：以環境執行真正的 vLLM 模型。重啟時本機 vLLM 已佔用 GPU 記憶體，未同時載入第二份模型。
- **問題（與本功能無關，既有）**：agent 重啟後，本機服務 `vllm-gemma-local` 重新啟動，但 4 分鐘以上停在讀檔：`sample` 顯示主執行緒卡在 `open()`，日誌停在 `non-default args`，`/health` 無回應。同一時間在我的互動 shell 讀取同一模型目錄正常。這與先前「更新後 vLLM 重載卡住」相同，疑為 launchd 背景程序存取 NFS 的差異（與 Go 直連 LAN 的 no route to host 可能同源），尚未證實，也未改動掛載或網路。

### 2026-10-16 NFS 讀取卡住的唯讀診斷

- 結論（有日誌證據）：卡住的是 macOS TCC 的「網路卷宗」權限提示，不是 NFS 或網路。vLLM 日誌在 14:35:12 停止的同一毫秒，`tccd` 記錄 `kTCCServiceSystemPolicyNetworkVolumes`：已存在授權的 code requirement 與目前 cdhash 不符（`status: -67050`），隨即 `AUTHREQ_PROMPTING` 與 `display_prompt`（受 `com.maccluster.desktop`、responsible 為 `maccluster-agent`）。之後沒有 `AUTHREQ_RESULT`，提示一直未被回答，`open()` 因此永遠阻塞。
- 為什麼互動 shell 正常：授權是依「負責程序」判定；Terminal／Claude 工具已有授權，launchd 啟動的 agent 子程序沒有。
- 為什麼之前能跑、更新後卡住：授權綁定 code requirement；`scripts/build-app.sh` 以 ad-hoc 簽章，每次重建 cdhash 都變，舊授權失效，下一次存取 NFS 又觸發提示。這解釋了「更新後 vLLM 重載卡住」與本次重現。
- 佐證：NFS 掛載為 hard、vers=3、TCP 2049 可連；卡住時 `lsof` 沒有開啟任何模型檔，`sample` 主執行緒停在 `open()`；NFS 核心日誌無錯誤。
- 未驗證：Go／kubectl 直連 LAN 的 no route to host 是否為同類機制（Local Network 權限）。尚未檢查。
- 未做任何修改：沒有動掛載、網路、簽章或授權設定，也沒有操作該提示。

- 復原驗證：使用者在畫面上允許提示後，`tccd` 於 14:40:47 記錄 `AUTHREQ_RESULT authValue=2`（允許）。同一個 vLLM 程序（PID 34231，restarts 0）自行繼續載入，約 1 分鐘內日誌出現 `Application startup complete`，`/health` 回 200；agent 與 80 叢集維持 Ready。未重啟任何服務。授權仍綁定目前的 ad-hoc cdhash，下次重建會再失效。

## 2026-10-16 vllm-metal 環境載入真實 vLLM（gemma-3-4b-it）

- 模型庫沒有 E2B，E4B 與本機 `vllm-gemma-local` 相同（16 GB），改用完整的 `google/gemma-3-4b-it`（約 8.6 GB）。使用者指示：保留本機服務、不重啟 agent、記憶體壓力惡化就縮回 0。
- 臨時 Deployment `vllm-env-gemma3`（`maccluster-test`，副本 1，Recreate；label `environment: vllm-metal`、nodeSelector `env.maccluster.example.com/vllm-metal`、`command: [vllm]`、`HF_HUB_OFFLINE=1`；VLLM_PLUGINS 只來自環境登記）。
- 約 225 秒後 Pod Ready。日誌：載入 `metal` platform plugin、`Platform plugin metal is activated`、`Gemma3ForConditionalGeneration`。`/v1/models` 回 `gemma3-env`；`/v1/chat/completions` 回覆 `Kubernetes is an open-source system for automating deployment, scaling, and management of containerized applications.`（19 prompt／22 completion tokens）。這是 smoke test，不是效能評測。
- 記憶體：`memory_pressure` 的 free percentage 基準約 52%，載入中最低 29%，推論後單次讀到 8%（觸發縮回 0 的條件），縮回後讀到 85%。該指標波動很大，不應當作精確容量；pageouts 全程僅增加約 520。本機 `vllm-gemma-local` 全程 `/health` 200，未受影響。
- 縮回 0 後 Pod 與程序皆清除，Deployment 已刪除。`examples/vllm.yaml` 已改為環境版（label、nodeSelector、裸命令 `vllm`），**該範例（E4B 模型）本身未在此輪實際套用**，驗證的是同一機制搭配 gemma-3-4b-it。
- 記憶體 admission 仍不存在：第二份模型能否並存只靠人工判斷，見 todo。

### 2026-10-16 vllm-metal 環境第二次測試（gemma-3-4b-it）

- 條件同第一次：不重啟 agent、記憶體壓力惡化就縮回 0。`vllm-env-gemma3` 約 190 秒 Pod Ready；Metal plugin 啟用，`Gemma3ForConditionalGeneration`。三次相同的 chat 請求都回覆同一句正確答案（各 22 completion tokens），首次 5.65 秒，其後約 0.8 秒。
- 記憶體：載入前 free 92%；載入後與推論期間降到 14–15%（低於 20% 門檻，連續三次）；swap 用量未增加（約 20.5 GB 不變），pageouts 只增加約 180。依規則縮回 0 並刪除 Deployment，Pod 與程序清除。
- 發現：測試開始時本機服務 `vllm-gemma-local` 已不存在。`local-services.json` 在 15:14:14 被改為 `{}`，agent 未重啟（PID 34229 不變），我沒有發出刪除；推測是使用者從 GUI 刪除。定義備份在 `.local/backup-env-phase1/local-services.json`，未自動重建。
- 因此這次沒有「與本機 vLLM 並存」的情境，記憶體數字不能代表兩份並存。

### 2026-10-16 重建本機 vllm-gemma-local

- 依使用者指示，用 `.local/backup-env-phase1/local-services.json` 的定義經 agent `create` 重建（原生、`vllm` 絕對路徑、gemma-4-E4B-it、`gemma-local`、restartPolicy Never）。新 UID `local-35d4608cefb0926cac2f85756eef8c7b`，舊 UID 與歷史不保留；一次性請求檔已刪除。
- agent 未重啟、執行檔未重建，TCC 授權有效，沒有再卡在 `open()`。約 3.5 分鐘後日誌出現 `Application startup complete`，`/health` 回 200，`/v1/models` 回 `gemma-local`，restarts 0，agent 與 80 叢集 Ready，記憶體 free 約 90%。我的輪詢腳本有解析錯誤，所以前 200 秒的 health 000 不是服務問題，而是尚在載入；實際就緒時間以日誌為準，約 3.5 分鐘。

### 2026-10-16 設定附屬面板、工具列與 hover

- 移除獨立 Settings Scene；工具列、App 設定選單及 Command+, 統一開啟 Dashboard 的設定 sheet，完成／Escape 關閉。
- 移除右上 Docker Desktop 開啟按鈕；四顆剩餘按鈕補上具體用途的原生 help tooltip。
- 叢集、Compose 群組、原生／Docker 工作列加入低對比 hover 背景與細邊線，尊重減少動態效果設定。
- 編譯與簽章通過；CUA 確認設定為 sheet、完成與 Escape 關閉、Command+, 開啟，以及工具列四顆按鈕 help 文字。工具未提供純游標移動 API，未錄製 hover 動畫或 tooltip 懸停畫面；相關修飾器已確認套用。未改變服務或使用者的日誌偏好。

## 2026-10-16 執行環境管理 UI 與內嵌終端機

- 決策（使用者）：先不做專案檔案管理；不提供建立 venv（使用者自行建立，環境可用於非 Python）；終端機要內嵌在 App。因此環境改為語言中立：`VIRTUAL_ENV` 只在根目錄含 `pyvenv.cfg` 時設定，新增 `--extra-path`、`MACCLUSTER_ENV`，移除 Python 版本顯示。
- 後端：`env list --json`（含損壞項目與 bin 內指令）、`env shell`、Instance／Workload 的 `environment`、本機服務 `create` 支援環境（本機 engine 現在也有環境目錄）。`go vet`、`go test -race ./...` 通過，新增測試涵蓋非 Python 環境、損壞項目、shell 規格與 `create` 驗證。
- shell 規格以真實 zsh 驗證：即使使用者的 `.zshrc` 會改 PATH，環境 bin 仍在 PATH 第一位，工作目錄、VIRTUAL_ENV、VLLM_PLUGINS 與提示字首正確。
- App：側邊欄「執行環境」區、環境詳情頁、新增／編輯表單、移除確認（列出使用中的工作）、本機服務表單的環境選擇、FAQ 條目。依賴 SwiftTerm 固定 1.11.2。SwiftTerm 1.19 在建置時需要 Xcode Metal Toolchain（`xcodebuild -downloadComponent MetalToolchain`），未安裝系統元件，改用不含 Metal 著色器的 1.11.2。
- 內嵌終端機實測（XCTest，使用真實 agent CLI 與 `.local/agent` 的 `vllm-metal` 環境）：輸入指令後 `which vllm`、`$VIRTUAL_ENV`、`$VLLM_PLUGINS`、`pwd` 皆正確，關閉後 session 結束；先排版成最終尺寸再輸入時，輸出在之後縮放視窗仍保留。**觀察到**：若終端機在輸出之後才第一次被縮放（80→104 欄），互動式 zsh 重繪會讓先前輸出消失；`sh` 與 `zsh -c` 不會。App 中視圖建立後立即排版，所以日常不會遇到，但屬於已知行為。
- 畫面驗證方式與限制：Computer Use 找不到這個 App（未登記為可操作的應用程式），shell 也沒有螢幕錄製權限，所以改用 SwiftUI 離屏渲染（`NSHostingView` 轉 PNG）檢視詳情頁、新增／編輯表單與終端。渲染發現並修掉表單內容超出寬度被裁切的問題。**未驗證**：實際視窗中的側邊欄「執行環境」區（離屏渲染不畫 List，空白）、點擊操作、拖曳縮放、NSOpenPanel 選擇資料夾、移除確認 alert、真實視窗中的終端機鍵盤輸入。
- 背景 agent 未重啟（避免 vLLM 重載與授權提示）；新 App 的 `env` 相關操作走新版 CLI，所以可用，但舊 agent 不認得 `environment` 欄位，本機服務選環境與「使用中的工作」需重啟 agent 後才生效。

### 2026-10-16 重啟背景 agent 與新版 environment 欄位

- 依使用者指示以 `launchctl kickstart -k` 重啟 agent（新版執行檔）。agent 與 80 叢集回到 Ready；本機 `vllm-gemma-local` 重新載入。
- 新 agent 上的本機服務建立測試：`environment: vllm-metal` 加裸命令 `python3`，輸出為 venv 內的 python、`VIRTUAL_ENV=~/.venv-vllm-metal`、`VLLM_PLUGINS=metal`、`MACCLUSTER_ENV=vllm-metal`；status 的 workload 含 `environment`。環境不存在、或裸命令但未選環境，兩者都被 `create` 拒絕（400）。測試服務已刪除，原 `vllm-gemma-local` 不受影響。
- 網路卷宗授權提示如預期再次出現（重建簽章使授權失效）：15:58:41 提示、使用者於 16:09:24 允許（`AUTHREQ_RESULT authValue=2`）。提示期間 vLLM 卡在讀模型約 10 分鐘；允許後同一程序（PID 46410，restarts 0）約 1 分鐘內 `/health` 回 200、`/v1/models` 回 `gemma-local`。這再次證實每次重建都需要人工允許，穩定簽章仍未處理。
- 仍未驗證：實際視窗中的 App 側邊欄「執行環境」區與點擊操作（見前一節）。

### 2026-10-16 工具列「執行環境」選單

- 依使用者要求，在右上角工具列（運行日誌之前）加入「執行環境」選單按鈕：「新增執行環境…」，以及每個已登記環境的子選單「檢視／編輯…／移除登記…」。側邊欄區塊與 `+` 選單的「新增執行環境」保留。
- 重建並重開 App（agent 未重啟）。以輔助使用 API 驗證：工具列出現新的 AXMenuButton（help 為「執行環境：新增、檢視、編輯或移除」），按下後選單項目為「新增執行環境…」、分隔線、`vllm-metal`（子選單含「檢視」「編輯…」「移除登記…」）。驗證時只展開選單並以 Escape 關閉，沒有執行任何項目。
- 未驗證：各項目實際點擊後的行為（開啟表單、移除確認）與視覺外觀（無螢幕錄製權限，離屏渲染不含工具列）。

### 2026-10-16 工具列滑鼠提示修正

- 工具列用途文字改由獨立、不可取得焦點的浮動提示視窗呈現，避免在工具列內被裁切；滑鼠離開、點擊或原視圖消失時關閉。提示不攔截點擊。
- 以 AppKit tracking area（activeAlways）偵測滑鼠進出，移除只允許 key window 顯示的限制；適用側邊欄切換、執行環境、運行日誌、重新整理、設定與 FAQ。
- `./scripts/build-app.sh` 通過，已重開 dist/MacCluster.app，背景 agent 未重啟。
- 驗證限制：Computer Use 的 scroll／drag 可定位到工具列，但目前截圖未捕捉到提示，不能據此宣稱實際 hover 顯示已驗證；仍需使用者以實體或遠端滑鼠移入確認。

### 2026-10-16 FAQ 的 YAML 修正

- 發現的問題：FAQ「容器與日誌」的片段缺少必填的 `managed: "true"`（`Translate` 會拒絕）；「原生執行」的 Pod template 缺少 `nodeSelector` 與 `tolerations`（節點有 NoSchedule taint，Pod 不會被排程）且自稱「需修改後使用」；沒有環境（`environment`）範例，也沒有套用步驟。
- 修正：新增 `examples/faq-docker.yaml`、`faq-native.yaml`、`faq-native-environment.yaml`（完整 Deployment）。FAQ 新增「套用 YAML」分頁（kubectl 步驟、必要欄位、三份範例與複製按鈕、Pending 排查；範例中的 Node 名稱會代入偵測到的本機 Node），並移除兩處不完整片段。
- 測試：`internal/cluster/examples_test.go` 讀取 `examples/*.yaml` 的每個 Deployment，要求 `Translate` 接受、selector 與 template label 一致、含原型 taint 的 toleration 與 host nodeSelector，環境範例同時選擇公布該環境的節點。Swift 測試要求 App 內的範例字串與檔案逐字相同。
- 真實叢集驗證（80 叢集、`maccluster-test`、獨立名稱）：三份範例逐一 `kubectl apply`。Docker 與 Native 範例都 Ready。環境範例在環境尚未登記時維持未排程的 Pending（nodeSelector 生效，沒有節點公布該 label）；登記 `my-env`（連結 `~/.venv-vllm-metal`）後被排程並 Ready，程序為 `~/.venv-vllm-metal/bin/python3 -m http.server`，HTTP 200。驗證後刪除三個 Deployment 並移除 `my-env`，確認 Pod、容器、程序與 Node label 都已清除；本機 vLLM 全程不受影響。
- 驗證方式限制：FAQ 分頁以離屏渲染檢視版面；實際視窗中的分頁切換、複製按鈕與選取複製尚未驗證。

### 2026-10-16 側邊欄、環境編輯與玻璃材質

- 隱藏側邊欄的「執行環境」區塊；工具列環境選單及 `+` 新增入口保留。
- 環境編輯改為單一 `EnvironmentEditor` sheet item 攜帶設定快照，表單 State 在初始化時填入；不再以分開的 editingEnvironment／editor 狀態與 onAppear 傳遞。Bin 欄位顯示目前實際值。
- 移除不透明漸層底色，改用 behind-window NSVisualEffectView；保留降低透明度的實色 fallback。面板與選取列採 clear Liquid Glass；工具列不再額外包一層玻璃，側邊欄不再包大型玻璃卡片。移除設定頁重複底層材質，環境 GroupBox 採共用玻璃面板；日誌保持可讀的底色。
- `scripts/build-app.sh` 編譯、打包及簽章驗證通過；已重新開啟 GUI，背景 agent 未重啟。
- Computer Use 實測：冷啟動未先檢視環境，直接從工具列 vllm-metal → 編輯，名稱、root、binDir、VLLM_PLUGINS 均已填入；取消後從 `+` 開啟新增，欄位為空且儲存停用。未儲存或修改實際環境資料。側邊欄僅叢集／本機服務；設定分頁可開關；最終版叢集選取列及標頭有玻璃邊緣、工具列可見，內容可閱讀。
- 靜態截圖確認版面及邊緣層次；沒有以影片驗證玻璃折射動畫或不同桌布下的效果。

### 2026-10-17 Skill：deploy-to-mac-node

- 新增專案 Skill `.claude/skills/deploy-to-mac-node/`（英文）：`SKILL.md` 說明控制平面如何把服務部署到這個 Mac 節點（選 executor、每份 manifest 必要欄位、部署與驗證步驟、支援／不支援清單、網路與容量限制、Pending 排查表、回報格式），並附 `assets/docker.yaml`、`native.yaml`、`native-environment.yaml` 三份範本。
- 範本納入 `internal/cluster/examples_test.go`（與 `examples/*.yaml` 同樣要求 translator 接受、可排程到原型節點）。測試也發現大寫佔位符 `ENV_NAME` 會被 translator 拒絕（環境名稱限小寫），因此範本改用合法的 `env-name`；忘記替換時 Pod 會停在未排程而不是悄悄跑錯。
- 真實叢集驗證（80 叢集、`maccluster-test`、獨立名稱，替換佔位符後逐步照 Skill 操作）：docker、native、native-environment（使用 `vllm-metal`）三份範本都 Ready。Skill 表格中的兩個症狀屬實：缺少 managed label → Pending 且訊息為 `explicit maccluster.example.com/managed=true is required during prototype testing`；未替換環境佔位符 → Pending、未指派節點，原因在排程器 conditions（`7 node(s) didn't match Pod's node affinity/selector`），`status.message` 為空。測試資源已全部刪除，無殘留容器或程序，本機 vLLM 不受影響。
- 未驗證：Skill 對其他 Claude 實例的觸發率與實際遵循度（沒有跑 skill-creator 的評測與描述最佳化流程）。

### 2026-10-18 說明頁重構與固定埠（多埠）規格

- **固定埠尚未實作。** 依使用者要求，只先完成規格與文件：`docs/fixed-ports.md`（label `maccluster.example.com/fixed-ports`，多個埠以 `_` 分隔，因為 label 值不能含逗號；agent 不分配埠、探測打宣告的埠、`MACCLUSTER_PORTS`、撞埠與第二副本的 Pending 規則）、`docs/planned/fixed-ports.yaml`、README 與 todo 條目。Skill 新增「Fixed ports (planned, not implemented)」並明確要求不得向使用者宣稱已支援。目前 agent 會忽略該 label。
- **起因：** 另一個 session 建立的 `agent-kv-vllm-metal` 寫死 `--port 8100`，agent 的就緒探測打的是分配的 51883，Pod 因此 READY 0/1，而服務本身（8100）正常；移除探測或改用 `$(MACCLUSTER_PORT)` 是目前的替代做法，沒有動該 Deployment。
- **說明頁：** 新增元件（`HelpComponents.swift`：卡片、事實列、callout、表格、含語法色與複製按鈕的 codeblock、可收合的範例）與六個分頁（`HelpPages.swift`），整段文字改為表格、短句與 YAML／bash codeblock；Docker、Native、Native 環境各有可複製的 YAML，另有標示「規劃中」的多固定埠範例。每份範例仍與 `examples/faq-*.yaml` 或 `docs/planned/fixed-ports.yaml` 逐字相同（Swift 測試強制），並由 Go 測試對 translator 驗證（規劃中的範例不在 Go 驗證範圍，因為功能尚未實作）。
- 驗證方式限制：以離屏渲染檢視各分頁版面；實際視窗中的分頁切換、展開收合與複製按鈕未驗證。

### 2026-10-18 固定埠（多埠）實作

- 依使用者指示實作 `docs/fixed-ports.md`（`maccluster.example.com/fixed-ports`，`_` 分隔，最多 8 個）。程式：`internal/cluster/runner.go`、`internal/runtime/{types,engine,fixedports}.go`；App 工作詳情在多埠時顯示「固定埠」。FAQ 的第 4 份 YAML 與 Skill 由「規劃中」改為已實作（`examples/faq-native-fixed-ports.yaml`，經 Go 測試驗證 translator 接受並與 FAQ 逐字相同）。
- `go test -race ./...` 通過，新增測試涵蓋 label 解析與各種非法輸入、多埠啟動與 `MACCLUSTER_PORT(S)` 展開、動態埠行為不變、外部占用時維持 Pending 且不計次數並可恢復、同引擎重疊埠被拒、探測打宣告的埠。
- 獨立 agent 端到端（`serve` 另一個狀態目錄，與真實 agent、叢集無關）：本機服務宣告兩個固定埠，真實 Python 程序在兩個埠都回 200，狀態列出兩個固定埠 `[18101,18102]`（JSON 欄位為 `fixedPorts`：原本命名為 `ports`，編譯 App 時發現與外部 Docker 容器的 `ports` 字串欄位衝突而改名；改名後重跑獨立 agent 確認 `fixedPorts=[18111,18112]` 且無 `ports` 鍵，並新增 Go 與 Swift 測試防止再撞名）；第二個服務要求重疊埠時 Pending（`fixed port 18102 is already in use by fixed-a`，restarts／failureCount 為 0）；刪除第一個後第二個自動啟動並服務；結束後無殘留程序或監聽。
- **未驗證：真實叢集。** 真實背景 agent（PID 46408）仍是舊版，會忽略此 label。生效需要重啟它：重啟會停止並重新啟動該 Mac 上所有原生工作，包括另一個 session 的 `agent-kv-vllm-metal`（vLLM 需重新載入模型，期間服務中斷），並因重建簽章再出現網路卷宗授權提示。依使用者「不要動 agent-kv-vllm-metal」的指示，沒有重啟，也沒有改動該 Deployment。vLLM 實際在固定埠就緒亦未驗證。

### 2026-10-18 固定埠：真實叢集驗證

- 依使用者指示（已通知 `agent-kv-vllm-metal` 的使用方）重啟真實背景 agent（`launchctl kickstart -k`，PID 46408 → 37594）。重啟前先備份該 Deployment 的 YAML 到 `.local/backup-env-phase1/`；沒有修改該 Deployment。agent 與 80 叢集回到 Ready。
- 輕量工作 `fixed-ports-check`（`maccluster-test`，native、`fixed-ports: "18121_18122"`、`readinessProbe` 打 18121，Python 在兩個埠各起一個 HTTP 服務）：Pod 約 16 秒 Ready；狀態 `port=18121`、`fixedPorts=[18121,18122]`；兩個埠都回 200；日誌顯示 `MACCLUSTER_PORT=18121`、`MACCLUSTER_PORTS=18121,18122`；兩個監聽皆在 127.0.0.1。這證明探測打的是宣告的埠，而不是分配埠（`agent-kv-vllm-metal` 的 READY 0/1 正是因為後者）。
- `replicas` 調為 2：第二個 Pod Pending，訊息 `fixed port 18121 is already in use by fixed-ports-check-84ff87457b-vbc8h`，agent 端 restarts 0、failureCount 0；第一個維持 Running 且持續服務；縮回 1 後第二個消失、第一個不受影響。
- 非法設定：探測埠不在清單（`readiness probe port is not in maccluster.example.com/fixed-ports`）與 docker executor 搭配 fixed-ports（`… requires maccluster.example.com/executor=native`）都 Pending 並帶明確訊息。
- 驗證後刪除三個測試 Deployment，無殘留 Pod、程序或監聽。
- `agent-kv-vllm-metal`：重啟後網路卷宗授權提示再次出現，使用者於 14:28:56 允許；vLLM 約 70 秒內 `/health`（8100）回 200、`/v1/models` 為 `gemma-4-e4b`。它的 Pod 仍是 READY 0/1，因為它的 Deployment 尚未加 `fixed-ports: "8100"` label（依指示未修改）；加上後（會重建 Pod）探測就會打 8100。
- 仍未驗證：vLLM 實際在固定埠由 agent 判定就緒（上述驗證用的是輕量 Python 服務）；本機服務建立表單尚無固定埠欄位（只能透過 `create` JSON 的 `fixedPorts`）。

### 2026-10-18 Liquid Glass 規範檢查與管理程式運行狀態

- **檢查依據**：Apple 對 Liquid Glass 的建議——玻璃屬於控制與導覽層，不套在內容層；不要玻璃疊玻璃；`.clear` 玻璃只用於有媒體背景且已加暗層的情境；優先使用系統元件（工具列、側邊欄、分段控制、`.glass`／`.glassProminent` 按鈕）。
- **發現並修正**：
  1. 工具列：自訂的 `GlassIconButtonStyle`（每顆按鈕再加互動玻璃）與 `+` 選單的 `interactiveGlass()`，疊在系統已提供的工具列玻璃上；側邊欄切換鈕也被自製的取代。改為標準 `ToolbarItemGroup` 與系統的側邊欄切換鈕（輔助使用樹確認為系統的 Hide Sidebar 加標準按鈕群組）。
  2. 設定與說明的分頁列：以 `.plain` 按鈕加互動式 `.clear` 玻璃自製的選取指示，玻璃可能蓋住分頁文字。改為原生 `Picker(.segmented)`（輔助使用樹確認為 `AXRadioGroup`「設定分類」）。
  3. 內容層（標題、狀態列、設定與 GroupBox 卡片）使用 `.clear` 玻璃面板：改為材質卡片（`contentCard()`，`.regularMaterial`）；sheet 標題改為純文字（`sheetHeading()`）。
  4. 側邊欄列選取使用互動玻璃：改為扁平填色（`SelectionHighlight`），因為側邊欄本身已是玻璃，玻璃不能取樣其他玻璃。
  5. `appScreen()` 不再替整個畫面強制 `.buttonStyle(.glass)`；動作列改由 `glassControls()`（`GlassEffectContainer` 加 `.glass`）與 `glassPrimaryAction()`（`.glassProminent`）明確指定。
- **設定 → 管理程式**：新增「目前狀態」。管理程式回應狀態請求時顯示綠色「運行中」加最後確認時間；否則顯示紅色「未運行」加原因。每 3 秒隨輪詢更新。以輔助使用 API 讀取確認：運行時為「運行中」；曾因狀態目錄錯誤出現「未運行」及連線錯誤原文。顏色由程式指定（綠／紅），沒有以螢幕畫面確認。
- **未驗證（重要）**：沒有取得實際視窗的畫面。Computer Use 找不到這個 App，`screencapture` 與 ScreenCaptureKit 都需要螢幕錄製權限而被拒絕，離屏渲染又不畫玻璃。因此「玻璃不再遮住分頁文字」「按鈕外觀正確」僅依規範與元件結構判斷，尚待你在實際視窗確認。已加入開發用截圖輔助（`DevSnapshot.swift`，只有設定環境變數 `MACCLUSTER_DEV_SNAPSHOT_DIR` 並收到 SIGUSR1 才會擷取自己的視窗；需要把螢幕錄製權限授予 MacCluster）。
- **我造成並已還原的副作用**：為了取得畫面，我曾把 App 複製到 `~/Applications` 並啟動；之後重開正式版時，App 因 LaunchAgent plist 指向的不是自己，用預設狀態目錄改寫了 `~/Library/LaunchAgents/com.maccluster.agent.plist`。執行中的 agent 與已載入的 job 不受影響（仍是 `dist` 的 agent、`--state-dir .local/agent`），但下次登入／重新載入會改用空的預設目錄而失去叢集連線。已將 plist 還原為相同內容（備份：`.local/backup-env-phase1/com.maccluster.agent.plist.rewritten-by-copy`），並確認重開 App 不再改寫；`~/Applications` 的副本已刪除。教訓：不要同時用不同路徑的 App 副本連同一個 LaunchAgent。

### 2026-10-18 App 圖示

- 使用者提供的 SVG（1024×1024，圓角方形 x=32、寬 960、rx=214，含光暈與陰影的模糊濾鏡）存為 `app/Resources/AppIcon.svg`，轉成 `app/Resources/AppIcon.icns`（16–512 pt 的 1x／2x，共 10 張），`scripts/build-app.sh` 複製進 `.app` 並在 Info.plist 加 `CFBundleIconFile=AppIcon`。
- 轉換方式：AppKit 內建 SVG 渲染保留透明但會丟掉模糊濾鏡，QuickLook（`qlmanage`）保留濾鏡但壓成白底。因此用 QuickLook 的色彩，加上與 SVG `clipPath` 相同的圓角方形當 alpha 遮罩，並還原邊緣與白底的混合（`scripts/make-icon.swift`、`scripts/make-icon.sh`）。檢查：圓角外四個取樣點 alpha 為 0，圓角內為不透明，細節（陰影、光暈、玻璃臉）與原圖一致。
- 重建並重開 App 後，以 `NSWorkspace.icon(forFile:)` 取得系統實際使用的圖示，確認是這張圖；簽章驗證通過；背景 agent（PID 37594）與 LaunchAgent plist（`--state-dir .local/agent`）未受影響。
- 未驗證：實際 Dock／Finder 顯示（沒有畫面權限）。圖形佔滿 960/1024，比 Apple 的 824/1024 範本大，macOS 26 可能會自行縮放或加底板；若在 Dock 中顯得比其他圖示大，需要在 SVG 加外圍留白再重新產生。

### 2026-10-18 圖示 v3、GitHub Actions 與個資檢查

- **圖示**：換成 `macCluster-icon-v3.svg`（右眼改為 Kubernetes 舵輪）。`scripts/build-app.sh` 現在每次建置都把 `AppIcon.icns` 複製進 `.app` 並刷新圖示快取；以 `AppIcon.sha256` 偵測 SVG 是否變動：本機會自動重新產生 icns，CI（環境變數 `CI` 存在）改為報錯，因為 QuickLook 需要圖形工作階段。已驗證三種情況：正常建置套用、SVG 變動後自動重新產生、`CI=1` 且圖示過期時失敗；最後還原為上傳的 v3 原檔。
- **GitHub Actions**（`.github/workflows/build.yml`）：`macos-26` runner（Xcode 26.6 為預設），依序個資檢查、`go vet`／`go test -race`、不需要畫面的 Swift 測試（ANSI、FAQ 範例同步、狀態解碼）、`./scripts/build.sh`，並上傳 artifact `MacCluster-macOS-arm64`（`ditto` 壓縮的 `.app` zip、單獨的 agent、SHA256SUMS，保留 30 天）。三個 action 固定在 commit SHA（checkout v7.0.1、setup-go v6.5.0、upload-artifact v7.0.1，皆發布超過兩週）。
- **以乾淨副本模擬 CI**（排除 `.gitignore` 內容、重新下載所有依賴、`CI=true`）：個資檢查通過、Go 測試通過、Swift 子集 5 項通過、完整建置成功並套用圖示、zip 約 26 MB。另外在沒有 Docker 的乾淨 PATH 下 Go 測試也通過。
- **未驗證**：workflow 沒有在真正的 GitHub 上執行過（沒有 repository）。模擬使用本機的 Go 1.27.2 與 Xcode 工具鏈，runner 會用 `go.mod` 的 Go 1.25.0 與 Xcode 26.6 的 Swift，這兩個差異只有實際執行才能確認。CI 不執行畫面相關的 Swift 測試（渲染、內嵌終端機、需要本機 agent 的測試）。
- **個資檢查**：掃描會被提交的檔案與建置出的二進位檔。**沒有**電子郵件、主機名稱、憑證／金鑰／token（唯一的 token 形字串是測試用的 `abcdef.0123456789abcdef`）、叢集節點名稱。**有並已清除**：(1) 家目錄路徑與使用者名稱（`/Users/<使用者>/…` 的模型與 venv 路徑，出現在 `examples/vllm.yaml`、`docs/validation.md`），(2) 以機器名稱命名的範例節點名稱（約 20 處，改為 `maccluster-node`，同步修改 FAQ 內建範例與測試），(3) 內網 IP（兩個控制平面、NFS 伺服器；改為文件保留位址 `192.0.2.x`／`198.51.100.1`）與 README 的 SSH 帳號。因節點名稱改變，`scripts/integration.py` 新增 `--node-name`。建置出的二進位檔檢查：沒有路徑、使用者名稱、建置目錄或簽章身分（ad-hoc、無 Team ID）。清理前的檔案備份在被忽略的 `.local/backup-env-phase1/pre-scrub/`。
- 新增 `scripts/check-privacy.sh`（CI 與本機皆可跑）：檢查家目錄路徑、已知個人識別字串、私有網段 IP、電子郵件、憑證金鑰內容；已用植入的路徑與信箱驗證能攔截。
- **仍在本機、不會被提交的敏感內容**：`.local/`（管理 kubeconfig、節點憑證、known_hosts、測試輸出與備份）、`.tools/`、`dist/` 都在 `.gitignore`。若用 `git add -f` 或改動 `.gitignore` 就會洩漏。另提醒：我複製過一次未清理的 `docs/` 內容到 `.local/backup-env-phase1/pre-scrub/`，那裡仍含原始值，同樣不要提交。

### 2026-10-18 日誌區塊高度與頁面捲動

- **問題**：工作詳情頁的日誌是「撐滿剩下的高度」，上面的資訊愈多就愈小。以預設視窗（1218×680）實際渲染量測，帶編號的日誌可見行數：Compose 容器 約 6 行、一般 Docker 容器 約 13 行、原生服務 約 26 行，各頁不一致，Compose 頁幾乎無法閱讀。
- **修正**：所有工作詳情頁（原生、Docker、Compose 容器、叢集工作）共用同一個高度規則 `LogLayout`：日誌高度 = max(20 行的高度, 捲動區高度 − 120 pt)，行高依「設定」中的字體大小與額外行距計算；因此與上方內容多寡無關，且各頁在同一視窗下完全相同。整個詳情頁放進可垂直捲動的容器，上方內容放不下時可捲動，開啟頁面與切換服務時預設捲到日誌處。
- **驗證**（離屏渲染真實的 Dashboard，測試 `LogHeightProbeTests`）：三種視窗大小下，Compose、Docker、原生三種頁面的日誌高度完全相同且不低於最小值，且頁面本身有可捲動容器：預設 1218×680 為 452 pt（約 28 行，原 6／13／26 行）、最小 960×628 為 400 pt（約 24 行）、高視窗 1218×1100 為 872 pt（約 56 行）。目視 Compose 頁的渲染圖，日誌約 29 行。
- **未改動／限制**：「運行日誌」彈窗、容器詳細資訊（Inspect）、資源使用狀況（Stats）與環境頁的終端機有各自的版面，未套用此規則。日誌自己的捲軸與頁面捲軸是兩層：滑鼠在日誌上時滾輪捲日誌，在其他區域時捲頁面。最少 20 行與保留 120 pt 是 `LogLayout` 的常數，可調。實際視窗中的捲動手感沒有驗證（離屏渲染不會捲動）。

### 2026-10-18 服務狀態顏色

- 新增 `StatusStyle.swift`：把後端與 Docker 產生的狀態對應到五種顏色，並以圓點加文字呈現（不只靠顏色）：綠＝運行中（Running／Ready）；橘＝進行中（Pending／Joining／Leaving／Restarting／Paused／Removing）；灰＝已停止（Stopped／Succeeded／Created）；紅＝異常（Failed／Error／Dead／Removed）；紫＝無法確認（Unknown、無法識別的新狀態，以及管理程式或 Docker 沒有回應時顯示的過期結果）。無法識別的狀態一律當作「無法確認」，不會顯示成健康的綠色。Compose 專案只有每個容器都運行時才是綠色，有任何異常為紅，其餘依序為紫／橘／灰。
- 套用位置：側邊欄的工作列、叢集列與 Compose 專案列；工作詳情頁的主要狀態（膠囊標籤）；叢集詳情頁與叢集工作頁的連線狀態；Compose 專案頁的各容器卡片。設定的「管理程式」運行狀態先前已是綠／紅。說明頁「一般與管理」新增「狀態顏色」對照卡。
- 驗證：單元測試涵蓋後端會產生的每個狀態都有明確的顏色、五種顏色與標籤互不相同、Compose 彙總規則、未知狀態不會顯示為健康。離屏渲染含五種狀態容器的 Compose 專案頁，目視確認紅（Dead）、橘（Restarting）、綠（Running）、灰（Stopped）、紫（Unknown）與紅色的專案摘要。
- 未驗證：側邊欄的實際顯示（離屏渲染不畫 List，側邊欄使用同一組元件但沒有直接看到）。側邊欄的「執行環境」區已被他人移除，因此環境沒有套用狀態顏色。

### 2026-10-18 液態玻璃按鈕、Docker 埠號與隱藏空的 PID／埠

- **按鈕套用液態玻璃**：彈窗層級的按鈕（取消、完成、重新讀取、複製 JSON、選擇…）原本因為先前避免「玻璃疊玻璃」而是預設樣式，現在 `appScreen()` 讓畫面上所有按鈕使用 `.glass`（沒有 macOS 26 或開啟「降低透明度」時退回 `.bordered`）；動作列維持 `glassControls()`（`GlassEffectContainer`），主要動作 `.glassProminent`，程式碼區塊的「複製」改為小型 `.glass`。工具列按鈕與側邊欄／Compose 卡片這類整列可點的導覽列維持系統行為。
- **驗證方式**：離屏渲染不畫玻璃，所以改用結構證據。`.glass` 按鈕會在視圖階層多出 `CASDFLayer`／`CABackdropLayer`（獨立按鈕 2 個、在 `GlassEffectContainer` 內每顆 1 個，`.bordered` 為 0）。測試 `GlassButtonStructureTests`（macOS 26.6 實測的數字，使用私有圖層名稱，OS 更新可能要調整）確認：各按鈕樣式可由圖層區分；`appScreen()`、`glassControls()`、`glassPrimaryAction()`、小型複製按鈕都產生玻璃圖層；真實的環境表單 5 顆按鈕有 10 個玻璃圖層，程式碼區塊的複製按鈕有 2 個。嘗試對整個 Dashboard 計數失敗（102 個，含側邊欄與系統層，不只按鈕），已移除該測試。**沒有看到實際的玻璃外觀**（沒有畫面權限）。
- **Docker 埠號顯示不出來**：`docker ps` 的 Ports 只列「目前正在發布」的埠，已停止（exited）或剛建立（created）的容器是空的；Docker Desktop 則顯示容器建立時設定的對應。實測：`open-webui`（主機 3000→8080）、`vllm_service_gemma4_26b`（8004→8000）、`open-notebook` 兩個容器都有設定但 `docker ps` 為空。修正：對「不在執行且 `docker ps` 沒有埠」的容器，用一次 `docker inspect` 讀 `HostConfig.PortBindings`／`Config.ExposedPorts`，以 `0.0.0.0:3000->8080/tcp` 格式顯示，並以 `portsConfigured` 標示，介面註明「容器未執行，顯示建立時的設定」。執行中的容器維持 Docker 的即時值；查詢失敗不影響清單。單元測試涵蓋格式、排序、隨機主機埠、非法 JSON、只對未執行容器查詢（假 docker 若被問到執行中的容器會報錯）與查詢失敗。以真實 Docker 驗證：上述各容器都顯示對應的埠，`ollama` 不變。這個修正走 App 內建的 `inventory` 指令，不需重啟背景 agent。
- **隱藏空的 PID／埠**：容器資訊頁的 PID 與埠只在有值時顯示（0 視為沒有），外部 Docker 容器因此不再寫「PID：— 埠：—」；原生服務與叢集工作有值時照舊顯示（多固定埠為「固定埠：…」）。

### 2026-10-18 工具列與設定頁沒有液態玻璃：根本原因與修正

- **現象**：使用者回報右上角工具列按鈕與設定頁都沒有正確的玻璃效果（即使前面已把工具列改成標準項目、分頁改成原生分段控制）。
- **診斷方法**：在開發用的自我檢查工具加入 SIGUSR2，傾印真實視窗的視圖與圖層結構（只在環境變數 `MACCLUSTER_DEV_SNAPSHOT_DIR` 啟用，不需要螢幕錄製權限）。修正前，主視窗的 `NSToolbarItemViewer` 底下**沒有任何玻璃圖層**，整條工具列只有標題區 1 個；也就是工具列根本沒有得到系統玻璃。
- **根本原因**：`vtool -show-build` 顯示執行檔的 `minos 14.0 / sdk 14.0`。SwiftPM 把部署目標當成連結時的 SDK 版本寫進 Mach-O；macOS 26 依「連結時的 SDK 版本」決定是否自動套用液態玻璃，低於 26 的 App 會被當成舊 App：工具列、側邊欄、sheet、分段控制、步進器、預設按鈕全部使用舊外觀。只有明確寫 `.glass`／`.glassEffect` 的元件有玻璃，所以我先前對按鈕的結構檢查能通過，卻漏掉了系統元件。
- **修正**：`scripts/build-app.sh` 以 `-Xlinker -platform_version -Xlinker macos -Xlinker 14.0 -Xlinker <SDK 版本>` 建置，最低系統仍是 macOS 14，但記錄真正的 SDK 版本（本機 27.0；CI 會用 runner 的 Xcode SDK）。建置後再用 `vtool` 檢查，若安裝的 SDK 是 26 以上但執行檔記錄的低於 26，直接讓建置失敗。
- **修正後的結構**（同一個傾印工具）：工具列出現系統的 `NSToolbarPlatterView`、`NSGlassContainerView`、`NSGlassEffectView`，視窗的玻璃圖層從 9 增加到 12；側邊欄改為系統玻璃側邊欄（`NSBlurryAlleywayView`）；設定頁（sheet）為系統的 `SwiftUISegmentedControl` 與步進器，圖層中有 7 個玻璃相關圖層（另有 1 個是自訂背景）。
- **未驗證**：視覺外觀。沒有取得實際畫面，只有結構證據。切換到系統玻璃後，工具列、側邊欄、sheet 的外觀會和之前不同，其中另一個工作階段加入的自訂透明視窗背景（`WindowBackdrop`，把視窗設為不透明度關閉）與系統玻璃搭配得好不好需要實際看畫面；sheet 是系統的 `SheetPresentationWindow`，但仍被自訂背景覆蓋，可能沒有系統 sheet 的預設玻璃底。

### 2026-10-18 `+` 選單調整與彈出視窗的液態玻璃

- **`+` 選單**：左上角「新增」選單移除「新增執行環境」，「加入叢集」改為「加入 Kubernetes 叢集」，提示文字改為「加入 Kubernetes 叢集，或新增本機服務」。執行環境仍從右上角工具列的「執行環境」管理。README 同步。
- **彈出視窗沒有玻璃的原因**：`appScreen()` 替每個 sheet 加了自訂背板（`AppCanvas`，一個 `NSVisualEffectView` 加色調層，並把視窗設成非不透明），蓋住了 macOS 26 sheet 預設的系統玻璃底。修正前傾印設定頁 sheet：視窗背景是我們設的透明灰色，系統的背板被蓋住。
- **修正**：`appScreen()` 不再加任何背景，只保留按鈕玻璃樣式與 GroupBox 卡片樣式。
- **驗證**（傾印真實視窗的結構）：設定、FAQ、運行日誌三個 sheet 都變成系統視窗背景（`Catalog color`），視窗框架下出現系統提供的 `BackdropView`，標題列出現系統的捲動邊緣效果（`NSScrollPocket`）；設定頁 sheet 的玻璃相關圖層從 9 增加到 12。所有 sheet 共用同一個修飾器，其餘表單（加入叢集、建立服務、環境表單、容器詳細資訊、資源使用狀況）同樣受益，但沒有逐一傾印。
- **未驗證**：視覺外觀。沒有看到實際畫面；sheet 內的內容卡片（`.regularMaterial`）疊在系統玻璃上的可讀性與觀感需要你確認。

### 2026-10-18 彈出視窗改為視窗內的液態玻璃面板

- **使用者回報**：附上設定頁畫面，彈窗是不透明的深灰色，沒有液態玻璃。截圖中工具列已是系統玻璃，彈窗本體與內部卡片、按鈕是扁平的。
- **基準對照**：做了最小的 SwiftUI 對照 App（同樣以 SDK 27 連結，只有一個視窗與一個什麼都沒自訂的 sheet）並傾印結構：標準 sheet 是 `SheetPresentationWindow`（非不透明）加 `NSVisualEffectView` 一般材質，按鈕是標準 `NSButton` 外觀，**沒有任何玻璃圖層**。也就是前一次移除自訂背景後，彈窗等於系統預設，而系統預設的 sheet 在 macOS 26 並不是明顯的液態玻璃。接著嘗試 `presentationBackground(.clear)` 加滿版 `glassEffect`：傾印顯示系統 sheet 仍保有 `NSVisualEffectView` 與背板，玻璃只是疊在扁平材質上，沒有改善。
- **修正**：彈窗不再使用系統 sheet，改由 `ModalPresenter.swift` 的 `ModalHost` 在主視窗內顯示：半透明遮罩加 `glassEffect(.regular, in: 圓角 28)` 的浮動面板（折射主視窗內容，與工具列同類）、陰影、`isModal` 輔助使用特性、Esc 與遮罩點擊關閉（作業進行中不可由遮罩關閉）。高度不超過視窗，放不下就捲動。內容改用自訂的 `modalDismiss` 環境動作取代 `@Environment(\.dismiss)`，取消鈕加上 Esc 快捷鍵。彈窗內的卡片從不透明材質改為半透明色調（`sheetPanel()`），避免蓋住玻璃。降低透明度時退回一般材質面板。
- **驗證**（傾印真實視窗）：打開設定、FAQ、運行日誌時不再有 `SheetPresentationWindow`（視窗數維持 1），主視窗的玻璃圖層由 12 增加到 19（面板、按鈕、分段控制）；按 Esc 後回到 12，確認可關閉。單元測試：`ModalHost` 產生玻璃圖層而純文字沒有，面板上的按鈕會增加玻璃圖層；全部 19 個 Swift 測試通過。
- **未驗證**：視覺外觀（沒有畫面權限）；表單類彈窗（加入叢集、建立服務、環境表單、容器詳細資訊、資源使用狀況）共用同一個 `ModalHost`，但沒有逐一傾印或操作；面板放不下時的捲動、遮罩下方內容的鍵盤焦點、`NSOpenPanel` 與面板並存的行為未實測。

### 2026-10-18 彈出畫面改為 Launchbit 的做法

- 使用者指定參考 `Launchbit`（專案目錄內另一個純 AppKit 專案，有自己的 `.git`）的彈出畫面做法。它的設定畫面是視窗內的覆蓋層：整個視窗上一層 28% 黑色遮罩；面板是 `NSVisualEffectView`（`.hudWindow`、`.withinWindow`、`.active`、強制 `darkAqua`），圓角 24、1 pt 白色 17% 邊框、陰影（黑色 0.42、半徑 28、下移 8）；寬度夾在 520–640、高度夾在 480–704（皆不超過視窗減 48），置中；「放開滑鼠」才關閉（按下與放開都在面板外）；彈窗期間吃掉拖曳、滾輪、右鍵等所有事件，不讓它們漏到下面的內容；面板高度設計成最長的分頁也不用捲動，視窗太小才退回捲動；檔案選擇與警示以 `beginSheetModal` 附在主視窗。
- 套用到 macCluster（`ModalPresenter.swift`）：面板表面改為同樣的 HUD 材質（強制深色，內容也用深色配色），圓角 24、白色 17% 邊框、同樣的陰影；遮罩改為 28%；寬度與高度不超過視窗減 48，放不下才捲動；每個彈窗有輔助使用標籤；彈窗開啟時底下的內容與工具列停用；檔案選擇（選擇資料夾）改為附在視窗上的 sheet。保留 `ModalSurfaceStyle.glass` 作為一行即可切換的液態玻璃版本。按鈕仍使用 `.glass`。
- 驗證（傾印真實視窗並以輔助使用讀取）：打開設定彈窗後，主視窗內出現 `NSVisualEffectView material=13（hudWindow） blending=1（withinWindow） state=1 appearance=DarkAqua`，與 Launchbit 的面板設定相同；視窗數維持 1（沒有 sheet 視窗）；工具列「運行日誌」「FAQ」的 `enabled` 為 false，按 Esc 後回到 true 並移除面板。單元測試確認預設面板是一個 within-window HUD 材質、純文字沒有，glass 版本才有玻璃圖層，全部 19 個 Swift 測試通過。
- **注意**：Launchbit 目錄含個人資料（家目錄路徑、已安裝 App 清單、`.claude-flow` 記憶檔）。已加入 `.gitignore`，個資檢查在無 git 時也略過它；我只讀取它，沒有修改其中任何檔案。
- 未驗證：視覺外觀（沒有畫面權限）；以 AppKit 實作的 Launchbit 面板與 SwiftUI 內容的細節差異（例如字型與間距）沒有逐一比對；表單類彈窗未逐一操作。

### 2026-10-18 設定與說明彈窗：原生分頁（切換動畫）與 Launchbit 版面

- **問題**：使用者要求與 Launchbit 一模一樣，並指出切換分頁時沒有動畫。Launchbit 的設定畫面程式裡沒有任何自訂動畫：它使用 AppKit 原生的 `NSTabView`（預設的 `topTabsBezelBorder` 樣式），切換動畫是系統分頁控制項自己的；它的覆蓋層開啟與關閉也沒有動畫（直接加入／移除視圖）。我先前用 SwiftUI 的分段選擇器，並以 `if` 直接替換內容，所以沒有動畫。
- **修正**：新增 `NativeTabView.swift`，以 `NSViewRepresentable` 包裝 `NSTabView`（強制深色外觀），每個分頁以 `NSHostingView` 放進 SwiftUI `ScrollView`（四周內縮 22 pt）。設定彈窗有「一般」「管理程式」兩個分頁，說明彈窗有六個分頁，說明彈窗不再有自訂標題（Launchbit 沒有）。版面照 Launchbit：分頁區左右內縮 22、上 24、下 72，右下角 120×30 的「完成」按鈕（右 25、下 24）；面板尺寸規則 寬 = min(640, max(520, 視窗寬 − 48))、高 = min(704, max(480, 視窗高 − 48))；設定內容標題 16 pt 半粗體、列間距 16、註解 12 pt。已移除我先前加的彈窗開關淡入淡出（Launchbit 沒有）。
- **驗證**（真實視窗，傾印加輔助使用）：設定彈窗內是 `NSTabView tabs=[一般, 管理程式] type=0`，說明彈窗六個分頁；以輔助使用按下「管理程式」後選取狀態改為管理程式且內容換成「目前狀態 … 運行中」，切回與說明頁切到「套用 YAML」也都正確換內容；Esc 關閉後視窗內沒有 `NSTabView`。單元測試確認是預設樣式的 `NSTabView`、一頁一個分頁、深色外觀、使用者切換會回報，以及尺寸規則的三個案例；全部 21 個 Swift 測試通過。
- **未驗證**：切換動畫本身。動畫是系統的 `NSTabView` 提供的，我只能確認用的是同一個控制項，無法在沒有畫面的情況下看到它的動畫。按鈕仍使用 `.glass`（Launchbit 的按鈕是 AppKit `.rounded` 樣式）；表單類彈窗沒有分頁，維持原本的內容版面。

### 2026-10-18 另一台 macOS 27 主機上分頁版面異常：加固與診斷工具

- **回報**：App 搬到另一台 macOS 27 的測試主機後，UI 分頁看起來怪怪的；本機是 macOS 26.6.1。沒有該主機的畫面或資料，**無法在這裡重現，原因尚未確認**。
- **判斷**：不是「用了 AppKit」本身（Launchbit 是純 AppKit 且目標就是 macOS 27）。風險在於把 SwiftUI 放進 `NSTabView` 的頁面：這個組合有兩處行為依賴系統版本，先前都使用系統預設：(1) 巢狀 `NSHostingView` 預設的 `sizingOptions`（本機實測為 7：把最小、理想、最大尺寸往外傳）與 `safeAreaRegions`（全部），(2) 包裝元件讓 SwiftUI 去問 `NSTabView` 的固有尺寸，而不是自己決定大小。
- **加固**（`NativeTabView.swift`）：分頁頁面的 hosting view 改為 `sizingOptions = []`、`safeAreaRegions = []`；元件以 `sizeThatFits` 直接填滿 SwiftUI 給的空間。本機實測：hosting view 的 `sizingOptions=0`、`safeAreaRegions=0`，每個分頁（設定 2 個、說明 6 個）切換後，被選取的頁面框架都等於 `NSTabView.contentRect`（590×506）。
- **診斷工具**：`scripts/dump-ui.sh` 在任何 Mac 上啟動 App、等使用者開到有問題的畫面，再輸出 `maccluster-ui-structure.txt`：OS 版本與 CPU、視窗與視圖階層、分頁容器的 `contentRect`／安全區域、hosting view 設定，以及 `CHECK` 行（被選取的頁面是否填滿內容區）。內容只有類別名稱與尺寸。
- **其他可能**（需要畫面才能判斷）：「分頁資訊」若指的是資料而不是版面，另一台主機沒有背景 agent 與叢集設定時，設定頁會顯示「未運行」、側邊欄沒有叢集，這是預期行為；該主機以 SDK 27 beta 連結的執行檔跑在正式版 macOS 27 上是否有 API 差異也未知。

## 2026-10-18 分頁改為純 SwiftUI（AnimatedTabView）

- 動機：另一台 macOS 27 測試機上 NSTabView 版本的分頁顯示異常，原因未能重現；改為不依賴 NSTabView 的純 SwiftUI 實作，避免各系統版本對 AppKit 控制項的尺寸與外觀差異。
- 實作：`AnimatedTabView`（頂部膠囊分頁列、`matchedGeometryEffect` 選取指示器、頁面依切換方向滑入並淡入、舊頁淡出；系統開啟「減少動態效果」時不播放動畫）。設定與說明彈窗改用它，`NativeTabView` 與其測試已移除；版面（22/24/72 邊距、完成按鈕）不變。
- 已實測：Swift 測試全部通過（含新增的 `AnimatedTabViewTests`：只建立選取頁、切換後才建立目標頁）；重建並重啟 app 後 SIGUSR2 結構傾印正常（macOS 26.6.1）；agent 未重啟。
- 未能驗證：實際視覺與動畫（無螢幕錄製權限，只有結構證據），以及 macOS 27 上的外觀，需使用者目視確認。

## 2026-10-18 Service 網路、主機資訊與 kubelet 試作

- **隔離範圍**：control plane Kubernetes v1.28.15；Mac macOS 26.6.1 arm64；專用 Node `maccluster-nettest-20261018`、namespace `maccluster-net-test-20261018`。Linux probe 與 EndpointSlice controller 在獨立 Linux worker 上運行。既有背景 agent、vLLM 與其他 namespace 工作未重啟／修改。
- **連線確認**：原 control plane TCP 6443 及 curl mTLS `/version` 成功，並非主機不可達。Go／kubectl 部分程式仍報 no route to host，原因未確定。使用僅監聽 loopback 的 nc 原始 TCP 轉接，kubeconfig 保留 CA 與 API server 名稱驗證；未使用 insecure-skip-tls-verify。轉接與憑證只存受保護的 `.local`，不提交。
- **Node**：Capacity 實測 14 CPU／48Gi；此測試 worker 刻意保留 13 CPU／44Gi，Allocatable 為 1 CPU／4Gi。InternalIP 為 Mac LAN IP。記憶體壓力實測 Warning → MemoryPressure=True；僅對輕量測試 Pod 加該 taint 的 toleration。PIDPressure=Unknown、NetworkUnavailable=True，不宣稱 CNI 完整就緒。
- **Service**：selectorless Service、專用 label/annotation 與動態 EndpointSlice；nginx Docker 與原生 Python HTTP 均由 Linux Pod 透過 Service DNS 成功存取。實際 Native HTTP 的 document root 是只含測試文字的專用資料夾；公開範例使用固定回應，不公開主機目錄。
- **副本與端點**：兩種 Deployment 各自 replicas 2 → 1 → 0 → 1，Ready Pods／EndpointSlices 數量同步為 4／2／0／2；每副本入口埠不同；有副本時 Service 都可達。
- **Mac 心跳中斷**：只 SIGSTOP 測試 worker，Linux controller 在 lease 過期（約 40 秒）撤除兩個 EndpointSlices；SIGCONT 後恢復並成功連線。Linux controller 已用 namespace ServiceAccount 運行，與 Mac worker 生命週期獨立。
- **Readiness**：只 SIGSTOP 專用 Native HTTP 程序，Ready 失敗且 EndpointSlice 撤除；恢復程序後端點重建並再次通過 Service HTTP 存取。
- **Kubelet**：24 小時 serving certificate 含 Mac IP SAN，listener 測試埠 10450。Docker／Native `kubectl logs --follow --tail=0` 均收到新 HTTP 請求紀錄；兩種 executor 的 Pod `port-forward` 均返回正確 HTTP 本文，Docker timestamps 成功。無 client certificate 被 TLS 拒絕；fake API／TLS 單元測試驗證錯誤 CA、SAR 拒絕、Pod UID／Node／container 不匹配及任意主機埠皆拒絕。
- **測試**：`go test -mod=readonly -race ./...` 全部通過。包含 TCP 半關閉／route 回收、controller opt-out／readiness／lease、mTLS／SPDY、Native log snapshot+subscribe 競態、重複行、輪轉、慢讀者、cancel，以及 Docker CLI 日誌選項。未進行 Swift UI 修改或 UI 視覺驗證。
- **部署狀態**：更新僅啟動於隔離 worker，未替換正式背景 agent／App。Linux controller 的現場驗證使用 Pod 加 binary copy；可重建映像的 Dockerfile 與 Deployment 範例已準備但未建置推送。正式 Node credentials 測試被自動核准審查拒絕：不允許在現有授權下簽發新的 client certificate 並新增 auth-review ClusterRoleBinding。相關 manifest 已準備，未套用。
- **保留與清理**：上述 namespace／測試 Node／Node Lease／serving CSR 暫留供確認，未清除。namespace 外有 `maccluster-network-test-read-nodes-20261018` ClusterRole/Binding，及 kube-node-lease 內 `maccluster-network-test-read-leases-20261018` Role/Binding。清理需先停止專用 worker，再刪專用 namespace、Node／Lease／CSR，以及上述專屬 RBAC；不可按廣泛 label 或刪整個 kube-node-lease namespace。待核准的 node-auth-review RBAC 尚不存在，不列為已建立資源。
- **仍未完成**：正常 Node identity 的端到端授權、正式 controller rollout、憑證續期、Secret/ConfigMap env、Metal admission、exec/metrics、startup/liveness、Mac 向叢集 DNS／ClusterIP 的網路。完整限制與設定見 docs/service-network.md。

## 2026-10-18 Linux worker 與 Mac 節點功能 A/B

使用者指定既有 control plane 與第一台 Linux worker，並授權 SSH 測試。本輪以 `maccluster-net-test-20261018` namespace 比較：Linux `nginx-linux-ab`（指定第一台 worker）、Mac Docker `nginx`、Mac Native `native-http`（後兩者使用隔離新版 `maccluster-nettest-20261018` 節點）。Linux 與 Mac Docker 都使用 `nginx:alpine` tag；這是跨架構功能驗證，不是同一 image digest 或效能基準測試。

| 項目 | Linux nginx | Mac Docker nginx | Mac Native HTTP |
| --- | --- | --- | --- |
| Control plane 主機直接存取 Service ClusterIP | HTTP 200 | HTTP 200 | HTTP 200 |
| Linux probe 透過 Service DNS + HTTP | 通過 | 通過 | 通過 |
| kubectl logs 快照 | 通過 | 通過 | 通過 |
| kubectl logs --follow --tail=0（識別新請求） | 通過 | 通過 | 通過 |
| kubectl port-forward + HTTP | 通過 | 通過 | 通過 |
| replicas 1 → 2 → 1、Ready endpoints 數量與 Service | 通過 | 通過 | 通過 |
| kubectl exec /bin/echo | 通過 | 404，尚無此路由 | 404，尚無此路由 |
| logs --timestamps | 通過 | 通過 | 明確拒絕；API server 對外呈現 InternalError |
| Node kubelet stats/summary | 有效 JSON | NotFound | 同一 Mac Node endpoint，不另重複請求 |
| Pod IP | 有 | 無 | 無 |

額外驗證：指定第一台 Linux worker 的 nginx Pod，可直接以 Service 名稱連到 Mac Docker 與 Native，兩者皆成功。在 control plane 的 SSH 工作階段內，主機自己的 kubectl 也成功讀到三個 Deployment 的新請求日誌。

Node 比較：Linux Capacity 4 CPU、8127784Ki 記憶體，PIDPressure=False、NetworkUnavailable=False；Mac Capacity 14 CPU、48Gi，測試 Allocatable 刻意保留為 1 CPU、4Gi，PIDPressure=Unknown、NetworkUnavailable=True。兩者 InternalIP 皆存在，當次 MemoryPressure／DiskPressure 皆為 False。Mac 雖獲分配 spec.podCIDR，但未有 Pod IP 或 overlay 資料平面，不把 CIDR 分配當成網路已完成。

範圍與限制：本輪沿用現有管理 kubeconfig，未簽發 client certificate 或套用待審核 node-auth-review RBAC，正常 Node identity 尚待驗證。Mac 出站叢集 DNS／ClusterIP、Secret／ConfigMap、volume、startup/liveness、GPU／Metal admission、metrics-server 本輪未驗證；stats/summary 成功不代表 metrics-server 已部署或 kubectl top 可用。

變更與保留：新增 Linux `nginx-linux-ab` Deployment／Service；三個 Deployment 最終皆為 1/1 Ready，測試 namespace 保留待使用者確認。既有服務未重啟，正式 App／背景 agent 未替換。原始逐項結果存本機 `.local/network-phase1/ab-results.json`，Node 結果存 `ab-node-results.json`，測試程式為 `ab-check.py`；均不含 kubeconfig／密碼且不提交版本控制。初始 Linux 對照短暫排在先前測試 worker，依使用者指定改排到第一台 worker後，才執行本節全部 A/B 檢查。

## 2026-10-18 網路測試分析：逐項唯讀檢查

全部為唯讀檢查，未修改叢集或 Mac 設定；叢集查詢只透過既有 SSH SOCKS 隧道執行 `get`。

1. **no route to host 根因（縮小，未最終確認）**：同一 shell、同一目標（control plane 6443）下，Apple 簽署的 `curl`、`nc`、`/usr/bin/python3` 連線成功；Go 編譯的最小 dial 程式、自行用 clang 編譯的最小 C `connect()` 程式、Docker 的 kubectl 全部回 `No route to host`。因此不是 Go 專屬，也不是路由問題（`route get` 走 en7、curl 成功）；與「非 Apple 簽署的二進位被拒絕連區域網路」的行為一致，高度疑似 macOS 本機網路隱私（TCC）。背景 agent 同機以 launchd 運行，status 為 Ready，說明該 App 路徑目前可連。尚未直接確認：TCC 資料庫需完整磁碟存取，未讀取；是否為 Claude 桌面 App 缺少本機網路授權，需使用者在「系統設定 → 隱私權與安全性 → 本機網路」檢視。
2. **Mac 出站 ClusterIP／DNS（已實測：不通）**：Mac 直連 Service ClusterIP（nginx 198.51.100.21、nginx-linux-ab 198.51.100.22）HTTP 逾時（curl rc=28）；對 kube-dns 198.51.100.10 的 dig 無回應；Mac 無 `cluster.local` 解析設定，ClusterIP 走預設閘道。結論：Mac 上的程序目前無法使用叢集 Service 名稱或 ClusterIP。這與 docs/service-network.md 的限制一致，現在有實測證據。
3. **Controller（靜態檢視）**：`internal/servicenet/controller.go` 每 3 秒輪詢；`Run` 在任何 Reconcile 錯誤時直接返回（需依賴 Deployment 重啟）；EndpointSlice 名稱固定，兩個副本同時運行會在 Create 競爭得到 AlreadyExists 而退出，故目前只能單副本；無 leader election。repo 只有 `deploy/network/Dockerfile`；controller-deployment.yaml 範例在 examples/network，尚需替換映像。映像尚未建置。
4. **憑證期限（重要發現）**：正式 Node `maccluster-example` 的 client certificate 到期 2026-10-22 06:13 GMT（約 3.5 天後），目前沒有自動續期（status 也註明）；到期後 agent 將失去叢集授權。測試 Node 的 kubelet serving certificate 到期 2026-10-19 10:05 GMT（約 15 小時）。Node identity 授權（node-auth-review RBAC）仍待核准，未套用。

建議優先順序：先處理正式 client certificate 續期，再做 Mac 出站 Service 方案與 controller 映像、leader election。

## 2026-10-18 Node client certificate 自動續期

- **實作**：`internal/cluster/rotate.go`。以節點目前的憑證身分建立續期 CSR（signer `kubernetes.io/kube-apiserver-client-kubelet`、7 天、相同 CN/O），簽發後驗證憑證與新金鑰相符、主體相同、壽命足夠，再原子替換 `node.kubeconfig`；舊檔保留為 `node.kubeconfig.prev`。只接受 `system:node:*`／`system:nodes` 憑證；任何驗證失敗都不改動現有憑證。agent 每 10 分鐘檢查（啟動後 20 秒首次），壽命用過 70% 或不足 24 小時即續期，成功後僅重啟該叢集 session（工作由引擎持有，不受影響）。續期不使用 bootstrap token 或管理員憑證，依賴叢集既有的 `kubeadm:node-autoapprove-certificate-rotation`。
- **單元測試**：`go test -race ./...` 全過；新增測試涵蓋時機判斷、成功替換與保留舊檔、檔案權限 0600、非節點憑證、金鑰不符、被拒、主體不符（失敗時憑證檔不變）。
- **叢集實測（唯讀＋1 個 CSR）**：對 `.local/agent/node.kubeconfig` 的副本（只在副本加上 SOCKS 代理設定，因本 shell 的 Go 程式無法直連 LAN）執行一次真實續期。建立 CSR `maccluster-example-renew-s4jt9`，由叢集自動核准並簽發（Approved,Issued）；新憑證有效期 2026-10-18 10:50 → 2026-10-25 10:50 GMT。以新憑證 `kubectl get node maccluster-example` 成功（Ready），身分為 `system:node:maccluster-example`、群組 `system:nodes`；舊憑證（.prev）仍有效。正式 `node.kubeconfig` 雜湊未變，正式 agent 未重啟，未修改任何 RBAC；副本與其私鑰已刪除。CSR 由叢集依預設保留期自動清除。
- **尚未做**：新 agent 尚未部署到正式環境，因此目前正式憑證仍會在 2026-10-22 06:13 GMT 到期；部署時機需使用者決定（重啟 agent 會使本機 vLLM 重載並再次出現網路卷宗授權提示）。續期後 session 重啟的現場行為、續期失敗時的使用者可見狀態（目前僅寫 stderr）、kubelet serving certificate 的續期（測試 Node 的 24 小時 serving 憑證）均未驗證或未實作。

## 2026-10-18 部署續期版 agent

- 以 `scripts/build.sh` 建置，經 `launchctl kickstart -k` 重啟背景 agent（state-dir 不變，仍為 legacy profile）。新 agent 回報 Ready，狀態訊息已更新；Native 工作 `agent-kv-vllm-metal` 等照常列出。本機 vLLM 因 agent 重啟而重載，網路卷宗授權提示由使用者處理。
- **更正**：先前說明「現有憑證已超過 70%」不正確。正式憑證 2026-10-15 06:13 → 10-22 06:13 GMT，部署時只用掉約 46%、剩餘 > 24 小時，所以**按設計尚未續期**（部署後約 3 分鐘觀察，沒有新 CSR、`node.kubeconfig` 未變）。依規則第一次續期預計在 2026-10-20 約 03:49 GMT（台北 11:49）之後的下一次 10 分鐘檢查。
- **未驗證**：agent 自行觸發的續期（含續期後 session 重啟）尚未在正式環境實際發生；目前只有單元測試與副本實測。續期失敗只寫 stderr，而 LaunchAgent 沒有設定 log 路徑，失敗目前看不到。

## 2026-10-18 連接埠與容器資訊統一顯示

- **問題**：叢集 Pod（Docker 執行）只顯示一個「埠：N」，本機 Docker 容器顯示 `docker ps` 原文；容器資訊是一整塊換行文字；UI 稱「外部 Docker」。
- **變更**：agent 新增 `portMappings`（`docker ps` 記法）：叢集 Docker Pod 由 inspect 的實際發佈埠產生，本機容器沿用既有的 `docker ps`／建立時設定（停止的容器）。App 新增「連接埠」卡片（本機埠／可連線範圍／容器內埠）與「容器資訊」卡片（映像、容器 ID 與複製按鈕）；IPv4／IPv6 同一對應合併為一列，127.0.0.1 顯示「僅此 Mac」、0.0.0.0／:: 顯示「所有網路介面」，只由映像宣告而未發佈的埠另行註明；原生程序（固定埠或動態分配）使用同一卡片。「外部 Docker 容器」全面改稱「本機 Docker 容器」（含側欄標籤與過期提示）。
- **已實測**：本機真實 Docker 的 5 個容器（含 3 個已停止、1 個 created 與雙埠容器）inventory 輸出的埠與 Docker Desktop 一致；Swift 26 項、Go `-race` 全過（新增 `PortRowTests`：IPv4／IPv6 合併、127.0.0.1／指定位址／範圍／僅宣告、Pod 與本機容器同列、舊 agent 缺 `portMappings` 的後備、原生固定／動態埠、無埠）。
- **未驗證**：實際畫面外觀（無螢幕錄製權限）；叢集 Docker Pod 的 `portMappings` 需新 agent，尚未重啟正式 agent，目前 App 對舊 agent 以 `port` 後備顯示「僅此 Mac」一列但無容器內埠，待下次重啟 agent 後才完整。

## 2026-10-18 叢集資訊頁整理

- 叢集詳情改為「連線資訊」卡片（狀態、Endpoint、節點、此 Mac 的工作數、管理權限；Endpoint 與節點用等寬字）；節點回報訊息與管理權限說明改用提示框（未設定時為警告色）；按鈕不變。純版面調整，未改資料與行為。
- 已實測：release 建置通過、App 重啟（agent 未動）。未驗證：實際畫面外觀，待使用者目視確認。

### 2026-10-18 更正：點進詳情頁無回應

- **現象**：上一版（叢集資訊卡片、連接埠／容器資訊卡片）點進詳情後 App 無回應；`sample` 顯示主執行緒全部耗在 SwiftUI `sizeThatFits` 版面計算。
- **處理**：三張新卡片原用 `Grid`（加 `gridCellColumns`、整個容器 `.textSelection`、`frame(maxWidth: .infinity)`），改為固定寬度標籤的 `HStack`／`VStack`（`InfoRow`）；文字選取只加在值上。根因精確到哪一個修飾子未逐項隔離，只確認改寫後不再發生。
- **驗證**：重建後以 AX 逐一點選側欄全部 8 列（叢集、原生、本機容器、Compose 等）並各 `sample` 1 秒，主執行緒皆在等待事件（約 98% 以上為 `mach_msg2_trap`），無版面迴圈；Swift 全部測試通過。未驗證：實際畫面外觀；Compose 展開後的子容器列未逐一點選。

## 2026-10-18 Compose 資訊整理

- 容器詳情的 Compose 專案／服務／工作目錄／設定檔併入「容器資訊」卡片（映像、容器 ID 之下以分隔線區隔，路徑用等寬字，附標籤可能已過期的說明）；原獨立的「Docker Compose」方塊與「非 Compose 容器」提示移除；「本機 Docker 容器 · 手動啟停」移到標題下的副標題。Compose 專案頁新增「專案資訊」卡片（專案、工作目錄、設定檔）。
- 驗證：release 建置通過，重啟 App 後逐列點選側欄，主執行緒皆閒置（無版面迴圈）。未驗證：實際畫面外觀。

### 2026-10-18 更正：點叢集資訊後整個畫面空白

- **現象（使用者截圖）**：選取叢集後側欄與詳情頁都空白，只剩工具列；視窗可拖動。主執行緒取樣為閒置，所以不是 CPU 卡死，先前「逐列取樣」的檢查方式看不出這種問題。
- **根因（已二分確認）**：叢集資訊頁使用 `HelpComponents.Callout`。它以 `fixedSize(horizontal: false, vertical: true)` 固定高度，放在詳情欄的一般 VStack 時，整個詳情欄（含上方標題卡）都不繪製；說明頁因在 ScrollView 內所以沒事。以簡化內容與只留連線資訊卡片對照，均能正常顯示；加入 Callout 即空白。
- **處理**：叢集頁改用本檔內的 `ClusterNote`（不含 fixedSize），共用的 `Callout` 不動。
- **驗證方式（新）**：`MACCLUSTER_DEV_SNAPSHOT_DIR` 模式的 SIGUSR2 傾印現在額外輸出 `window0.png`（以 App 自身 `cacheDisplay` 繪製，不需螢幕錄製權限；玻璃與模糊材質不會畫出，文字與版面會），並新增 `MACCLUSTER_DEV_SELECT=cluster|<本機容器名稱>` 啟動後自動選取，無需點擊即可重現頁面。修正後的叢集頁完整繪製（連線資訊、節點回報、說明、兩個按鈕）；本機 Docker 容器頁（open-webui）亦正常繪製（頁面依設計捲到日誌，卡片在上方）。
- **未驗證**：玻璃與動畫等實際視覺；Compose 專案頁與原生工作頁未以此方式逐頁截圖；macOS 27 機器未驗證。

## 2026-10-19 隔離 agent 實測與網路環境檢查

- **隔離方式**：新版 agent 編譯到 scratchpad（不寫入 `dist/MacCluster.app`，避免現行 agent 被 launchd 重啟時換成未驗證版本），以全新狀態目錄執行，僅用命令列操作（不經 App，避免 LaunchAgent plist 被改寫）。macOS Unix socket 路徑上限使狀態目錄需用短的符號連結路徑（`/tmp` 下，測完已刪）。
- **互不干擾（已實測）**：隔離 agent 有獨立安裝 ID、socket 與鎖；測試前後 Docker 容器清單（含已停止者）唯一差異是隔離 agent 自己建立的測試容器，關閉後清單與測試前快照完全相同；現行 agent（pid 3329）持續運行、`node.kubeconfig` 雜湊未變、原生工作仍為 Running。
- **`portMappings`（已實測）**：隔離 agent 建立本機 Docker 服務（本機已有的 nginx:alpine，無下載），狀態回報 `portMappings: "127.0.0.1:<埠>->80/tcp"`，loopback 存取 HTTP 200。
- **關閉流程（已實測）**：對隔離 agent 送 SIGTERM 後，其測試容器被移除，與程式碼行為一致（正常關閉會 `docker rm -f` 自己管理的容器）；現行 agent 的容器未受影響。尚未做「關閉時保留容器」的改動。
- **網路檢查（唯讀）**：control plane TCP 6443 與 HTTPS `/version` 均成功（200）；叢集 10 個 Node 中除 `maccluster-nettest-20261018` 外皆 Ready，正式 Mac Node Ready；到 control plane 的路由走 en7。`maccluster-nettest-20261018` 為 NotReady：其 `run-node` worker 程序已不存在，屬先前隔離測試遺留（待清理清單內）。
- **同一叢集多節點（程式碼檢視，未實測）**：同一個 agent 對同一 API server 只允許一份叢集設定（重複加入回 409）；另開 agent（不同狀態目錄與安裝 ID、不同 `maccluster-` 開頭的節點名稱）可加入同一叢集。限制：各 agent 各自回報整機容量（不跨 agent 分攤，會超額登記）、kubelet／固定埠需錯開；需要 bootstrap token（叢集寫入，需明確授權）。

## 2026-10-19 原生程序接管（agent 當機／重啟後銜接）與日誌改寫檔

- **問題**：agent 當機後留下的 `*.native` 標記使整個引擎拒絕啟動，需人工清理；程序輸出經管線交給 agent，agent 不在時輸出遺失，程序還可能因 SIGPIPE 被終止；日誌超過 1 MB 整份清空；服務重建前 `Logs()` 回 unknown UID。
- **做法（A：標記檔加強後的接管）**：
  - 原生程序的 stdout／stderr 直接寫日誌檔（無管線）；agent 只跟隨該檔（200 ms 輪詢）餵給既有的 follow 串流。
  - 標記檔改為 JSON：UID、名稱、PID、`ps` 的啟動時間與完整命令列、spec 雜湊、埠、啟動時間；先寫「launch intent」再寫完整記錄。
  - agent 啟動時逐一核對標記：程序仍活著且身分（PID＋啟動時間＋命令列）相符 → 接管（以 kqueue `NOTE_EXIT` 監看結束，後續 `Ensure` 沿用同一程序，spec 雜湊不符則拒絕且不動該程序）；程序已不在或 PID 已被別的程序重用 → 清除標記、不終止該程序；無法驗證的記錄（舊格式或僅有 intent）→ 只封鎖該 UID，其餘服務照常，不再讓整個引擎拒絕啟動。
  - 日誌輪替：超過 `logRotateAt`（預設 1 MB）時複製到 `<log>.1` 再截斷（copytruncate），保留前一代；`Logs()` 與串流快照讀「前一代＋目前檔」，且在沒有對應服務紀錄（停止、尚未重建、agent 剛重啟）時仍可讀歷史，只有完全沒有日誌檔的 UID 才是 unknown。
- **已實測（`go test -race ./...` 全過，新增 `native_detach_test.go` 8 項）**：當機後接管同一 PID 且不重複啟動並可停止；agent 不在期間輸出的日誌保留；接管後的程序結束會被偵測（狀態 Failed，原因 `ExitedWithUnknownStatus`）；PID 身分不符時不接管也不終止；死掉的標記被忘記並重新啟動；spec 不同則拒絕；僅 intent 的記錄只封鎖該 UID；輪替保留前一代、停止後與重啟後仍可讀歷史。既有測試的「當機標記拒絕啟動」改為「只封鎖該 UID」。
- **隔離 agent 端到端（不動現行 agent）**：建立每秒輸出的原生服務，`kill -9` agent 後子程序仍存活、標記仍在；重新啟動 agent 後接回同一 PID（Running），日誌檔連續；之後正常 SIGTERM 會停掉該程序並清掉標記。現行 agent（pid 3329）、其 `node.kubeconfig`、Docker 容器清單前後相同。
- **限制**：接管後的程序結束時無法取得結束碼（記為失敗、原因未知）；輪替為 copytruncate，複製與截斷之間寫入的少量位元組可能遺失；程序啟動前、標記寫完前就當機的極小視窗只能靠封鎖該 UID 處理；正常關閉（SIGTERM）仍會停止並移除所管理的程序與 Docker 容器（本次未改）；Docker 容器日誌仍隨容器刪除而消失。
- **未部署**：現行 agent 仍是舊版（管線日誌、當機後拒絕啟動）；此版需重啟 agent 才生效，重啟會中斷其管理的工作（含 vLLM 重載）。資料目錄搬到 `~/Library/Application Support/MacCluster`：預設值本來就是該處，現行安裝因 LaunchAgent plist 指向專案內 `.local/agent` 而不同；遷移（複製、改 plist、重啟）尚未進行。

## 2026-10-19 部署新版 agent 並遷移資料到 Application Support

- **步驟**：備份 `.local/agent` 與 plist（`.local/backup-agent-20261019*`，含憑證，不進版本控制）→ `scripts/build.sh` 重建 → `launchctl bootout` 正常停止現行 agent（約 1 秒收乾淨，標記檔已清除、受管工作已停止）→ 複製狀態目錄到 `~/Library/Application Support/MacCluster`（權限 700／檔案 600，略過 socket 與鎖檔，53 個檔案，差的 3 個是鎖檔）→ PlistBuddy 將 `--state-dir` 改為新位置 → `launchctl bootstrap` → 重開 App。
- **驗證**：新 agent（pid 79639）以新目錄運行；`status` 為 Ready，節點 `maccluster-example` 於叢集中 Ready；`agent-kv-vllm-metal` 工作重建為 Running（新 PID，固定埠 8100）、Pod 因模型載入暫為 0/1；標記檔已是新 JSON 格式（含 PID、啟動時間、命令列、spec 雜湊）；執行環境 `vllm-metal` 保留；節點 client certificate 沿用（到期 2026-10-22 06:13 GMT，新版續期邏輯已生效）；Docker 容器清單與遷移前相同；App 重開後顯示叢集資訊（舊 agent 已停止，所以是讀新目錄）。
- **沿用／保留**：舊 `.local/agent` 暫時保留作備份（內有節點憑證，確認穩定後應清除）；叢集管理用的 kubeconfig 是 profile 內指向 `.local/admin.kubeconfig` 的路徑，不在狀態目錄內，這次未搬。
- **影響**：本機 vLLM 因重啟而重載（模型載入中）。
- **未驗證**：新版 agent 的自動續期尚未實際觸發（預計 2026-10-20 約 03:49 GMT 之後）；vLLM 重載後 Pod 轉為 Ready 的時間。

## 2026-10-19 以新版程式重跑網路測試（含原生日誌 follow 回歸與叢集內接管）

- **範圍**：只動測試 Node `maccluster-nettest-20261018` 與測試 namespace；現行 agent 未動。測試 worker（`run-node`，狀態目錄 `.local/network-phase1`）換上新版二進位後重啟（舊版保留為 `maccluster-agent.old-codex`）。本機中繼 `relay.py`（僅 loopback，透過 Apple 簽署的 `nc` 連 control plane）需先啟動，因本 shell 的 Go 程式無法直連 LAN。
- **處理的殘留**：舊 worker 被終止後，其子程序（原生測試 HTTP 服務，父程序已為 1）仍存活，標記為舊格式；已確認是測試服務後停止它並清除舊標記（舊格式標記依設計只封鎖該 UID，需人工核對）。
- **結果（全通過）**：`logs --follow --tail=0`（原生與 Docker）、`port-forward`＋HTTP、`--timestamps`（Docker）；副本 2→1→0→1 的 Ready Pod／EndpointSlice 數量、唯一埠與 Linux→Service→Mac TCP；readiness 失敗撤除端點與恢復；暫停 worker 使 lease 過期後端點被撤除、恢復後重建；新版對 Docker 測試服務也能依名稱與擁有者標籤認領。
- **腳本問題（非回歸）**：`check.py`／`lease-check.py` 在 Linux 對照工作負載（`nginx-linux-ab`）與 network-controller 加入後，Pod 與 EndpointSlice 計數的斷言過時而逾時；以只計算 Mac 測試 Pod／`maccluster-` 開頭 slice 的副本（`check-mac-only.py`、`lease-check-mac-only.py`）重跑通過。原腳本未改。
- **叢集內接管實測發現並修正一個缺陷**：對測試 worker 做 `kill -9` 再重啟，原生測試服務的程序存活，但新 worker 另起新程序、舊程序成為孤兒。根因：身分比對包含命令列，而 `/usr/bin/python3` 是轉接殼，會再 exec 成 Xcode 內的真正 Python，啟動後命令列改變。修正為以「PID＋啟動時間＋程序群組 ID 等於 PID」為身分，命令列僅作診斷，並新增回歸測試。修正後重跑：worker 被 `kill -9` 後服務存活、標記保留，重啟後接回同一程序（未重複啟動），Pod 1/1、Mac EndpointSlice 2、Linux→Service→被接管程序成功，被接管程序的 `logs --follow` 與 `port-forward` 通過。`go test -race ./...` 全過。
- **重要**：現行 agent（今日部署）仍是含此缺陷的版本。影響僅限 agent 被 `kill -9`／當機後的接管（正常重啟本來就會停止並重建），且只在程序命令列於啟動後改變時失敗（直接以 `sh -c` 啟動的服務不受影響）。修正需在下次重啟 agent 時部署。
- **狀態**：測試 worker 目前運行中（Node Ready）；其 kubelet 服務憑證 2026-10-19 10:05 GMT（台北 18:05）到期，之後 `logs`／`port-forward` 測試需重簽。測試 namespace 與 Node 仍保留。

## 2026-10-19 Mac 節點與 Linux 節點功能比較

- 完整結果與建議見 [docs/node-comparison.md](node-comparison.md)。範圍：節點層級（Node 物件、kubelet 端點、Lease）與 16 項 Pod 功能，同一份規格分別落在 Linux worker、Mac Docker、Mac 原生；只動測試 Node 與測試 namespace，測完全部清理（Pod、ConfigMap、Secret、Docker 容器皆確認為 0）。
- 主要發現：不支援的規格被 API server 接受後無聲停在 Pending（只有 `status.message`，事件僅有 `Scheduled`，`logs` 回 not found）；Mac Pod 沒有 Pod IP、Pod 內無法解析叢集 DNS 或存取 Service；現行 agent 的 kubelet 端點未開；`kubectl exec`／`cp` 回 404；退避期間回報 Pending；`status.reason` 恆為 `MacClusterLocalExecution`。
- 已知測試限制：SIGTERM 項對 Linux／Docker 不完整（見文件）；`check.py` 類既有腳本的計數已過時，本輪另寫 `feature-ab.py`。

## 2026-10-19 階段 0：不支援規格的明確回報、狀態欄位與節點欄位

- **範圍**：只動程式與測試 worker／測試 namespace；現行 agent 未更新（仍是前一版）。新版二進位編譯到暫存檔後放入測試目錄，舊版保留為 `maccluster-agent.prev`。
- **實作**：`Translate` 一次列出**所有**不支援欄位（結構化 `SpecError`，原因 `UnsupportedPodSpec`／`InvalidPodSpec`／`NotManagedByMacCluster`）；Pod 維持 `Pending`，容器 waiting reason 為該原因，另送 Warning 事件（同一 Pod 同一訊息只送一次）。通知類：Native 接受 `hostNetwork`（Normal 事件）、Docker 拒絕；預設注入的 ServiceAccount token volume 不再讓 Pod 卡住，改為 Warning 事件 `ServiceAccountTokenNotMounted`。狀態欄位：`status.reason` 不再恆設；補 `started`、Docker `imageID`、`lastState`；`terminated.reason` 為 `Error`／`Completed`；崩潰退避時 Pod 為 `Running`、容器 waiting `CrashLoopBackOff`。節點：`PIDPressure` 依 `kern.maxproc` 與行程數量計算（≥90% 為 True）、`ephemeral-storage` 容量／allocatable、由硬體 UUID 雜湊衍生的 machineID／systemUUID（不暴露原始硬體識別碼）、`bootID`；刻意不填 `status.images`。
- **測試**：`go vet`、`go test -race ./...` 全過，新增涵蓋逐欄位訊息、誤用與不支援的原因區分、hostNetwork（Native 接受／Docker 拒絕）、token volume 忽略並警告、事件只送一次、狀態欄位形狀、節點欄位（含 machineID 穩定）。
- **回歸套件**：新增 `scripts/node-feature-suite.py`（每項對每個目標有預期結果，雙向判定）。對舊版 worker：36 項期望符合 13 項（紅）；對階段 0 版本：36 項全符合。使用者實際看到的訊息例：`env T02_PODNAME: valueFrom (fieldRef metadata.name) is not supported`、`volumes: "v" (emptyDir) is not supported; volumeMounts: "v" at /data`。
- **既有網路測試重跑（階段 0 版）**：副本 2→1→0→1、日誌 follow、port-forward、readiness 撤除與恢復全數通過。
- **未驗證**：事件在「正常 Node 憑證」下的建立權限（測試 worker 使用管理憑證）；`kubectl get pods` 在 Mac Pod 上的 STATUS 欄現在應顯示 `Running`（以狀態欄位判斷，未逐項截圖）；現行 agent 尚未部署，部署時需重啟（工作重建、vLLM 重載）。

## 2026-10-19 階段 1：環境變數來源、資源 limits、securityContext 子集

- **範圍**：只動程式與測試 worker／測試 namespace；現行 agent 未更新。新版二進位編譯到暫存檔後放入測試目錄（上一版保留為 `maccluster-agent.phase0`）。
- **實作**：`internal/cluster/envresolve.go`（kubelet 語意的環境組裝、fieldRef／resourceFieldRef／Secret／ConfigMap／`envFrom`、`$(VAR)` 展開）、`security.go`、`runner.go`（每個 Pod 的 Secret／ConfigMap 讀取快取：失敗不快取，Secret 事後出現會自動恢復；`CreateContainerConfigError`）、`internal/runtime`（`Spec.EnvResolved`/`Limits`/`Security`、Docker 參數、`OOMKilled`）。從叢集讀到的值不屬於 spec 的 JSON：不進雜湊、不持久化，Docker 以 `-e 名稱`＋CLI 環境傳遞（不使用 env-file，理由見路線圖）。
- **單元測試**（`go vet`、`go test -race ./...` 全過）：展開語意、環境來源順序與分類（含換行值與純字面值留在 spec）、Secret 不存在／key 不存在／optional、不可用來源的逐項說明、事件與快取、limits（Docker 強制／Native 警告）、securityContext 對應與拒絕、Native 只接受已成立的設定、`runAsNonRoot`、Docker 參數、解析後的值到達 Native 程序且不進雜湊與狀態檔。
- **回歸套件**（`scripts/node-feature-suite.py`）：54 項期望符合 53 項；唯一不符是跑到一半 Docker Desktop API 暫時連不上，單獨重跑 T18 通過。重點結果：Linux、Mac Docker、Mac Native 的環境變數輸出一致（含 `$(VAR)`、含換行的 Secret、prefix）；Secret 不存在時 Pod 為 `Pending`／`CreateContainerConfigError` 並有 Warning 事件，建立後自動啟動；記憶體超限在 Linux 與 Mac Docker 都是 `OOMKilled`，Mac Native 不強制（Warning 事件）；Docker 的 `runAsUser 1000:2000` 與唯讀根檔案系統與 Linux 一致，Native 說明「以 agent 的 macOS 帳號執行」。
- **Node 授權實測**（以身分模擬 `system:node:<節點>`）：被該節點的 Pod 引用的 Secret 可讀；沒被引用的 Secret 回 Forbidden；可建立事件。以正常 Node 憑證運行完整流程尚未實測。
- **既有網路測試**（副本縮放、日誌 follow、port-forward、readiness 撤除與恢復）重跑全通過。
- **未驗證／限制**：現行 agent 未部署；曝露面見 [docs/env-and-security.md](env-and-security.md)（`docker inspect`、`ps eww` 仍可看到值）；Secret 變更不會更新執行中的程式（與 kubelet 相同）；`limits` 未設時的 `resourceFieldRef limits.*` 不預設成節點可分配量，改為說明不支援。

## 2026-10-19 階段 2：探針、lifecycle hooks 與優雅停止

- **範圍**：只動程式與測試 worker／測試 namespace；現行 agent 未更新。新版二進位編譯到暫存檔後放入測試目錄（上一版保留為 `maccluster-agent.phase1`）。完整說明見 [docs/probes-and-lifecycle.md](probes-and-lifecycle.md)。
- **實作**：`internal/cluster/probes.go`（startup／liveness／readiness 狀態機，httpGet／HTTPS／tcpSocket／exec，成功與失敗門檻、初始延遲、週期；背景執行的 postStart 與 preStop；刪除 Pod 的背景停止追蹤）、`runner.go`（協調迴圈：刪除流程、探針事件、liveness 重啟只送一次）、`internal/runtime`（可選能力 `Execer`、`Killer`、`GraceStopper`；`Exec` 在 Docker 用 `docker exec`、在 Native 於程式的環境與工作目錄另起程序；`Kill` 與 `StopGrace` 不持有引擎鎖；kill 原因成為該次結束的 `LastExitReason`）。
- **修正既有缺陷（實測發現）**：Docker 刪除 Pod 原本 `docker rm -f` 直接 SIGKILL，程式從不收到 SIGTERM；Native 固定 2 秒後 SIGKILL；`Stop` 持引擎鎖等待，長寬限時間會卡住整個引擎。現在先 SIGTERM、等寬限時間再 SIGKILL，等待時不持鎖。
- **單元測試**（`go vet`、`go test -race ./...` 全過）：探針門檻與初始延遲、TCP／HTTPS（自簽憑證、標頭、User-Agent）／exec（結束碼、逾時、runtime 沒有 exec 能力）、liveness 達門檻才重啟且重啟後重算、startup 閘門（期間不跑 liveness、失敗達門檻會重啟）、postStart 閘門與失敗重啟、探針與 hook 對宣告埠的驗證與 grpc 的明確拒絕、reconcile 層級的「每次容器啟動只重啟一次、事件只送一次」與「preStop→優雅停止→移除 Pod」順序及剩餘寬限時間；runtime 層級的 Exec（環境、工作目錄、結束碼、逾時）、Kill（保留工作由重啟策略決定、SIGKILL 升級）、StopGrace（程式看到 SIGTERM、不阻塞其他工作）。
- **回歸套件**（`scripts/node-feature-suite.py`，24 案例×3 目標＝72 項期望）：全部符合。Linux、Mac Docker、Mac 原生的行為一致：exec／TCP readiness、liveness 失敗重啟（事件 `Unhealthy`＋`Killing`）、startup 閘門（期間 `started: false`、不 Ready）、postStart 執行與失敗重啟（`FailedPostStartHook`）、刪除時 preStop 先於 SIGTERM（preStop 期間容器仍在執行）、忽略 SIGTERM 的程式在約 5 秒寬限後才被終止。
- **測試方法的註記**：Docker Desktop 的 Alpine 映像裡，PID 1 為 shell 時 `trap` 不處理 SIGTERM（三種寫法都被拖滿寬限時間；Linux 節點相同），所以容器目標的 preStop／SIGTERM 順序以 nginx 當 PID 1、用「preStop 期間仍在執行、之後很快結束」的時間判斷，Native 以 trap 與標記檔判斷。
- **既有網路測試**（副本縮放、日誌 follow、port-forward、readiness 撤除與恢復）重跑全通過。
- **未驗證／限制**：現行 agent 未部署；探針在約 3 秒一次的協調迴圈內執行（精度與逾時行為受此限制）；`kubectl exec` 仍未實作；`preStop` 只在刪除 Pod 時執行；以正常 Node 憑證運行未實測；HTTPS 探針與 `sleep` hook 只有單元測試，沒有在叢集套件裡（`sleep` 需要 API 1.29 以上）。

## 2026-10-19 階段 4：kubectl exec／cp／attach、日誌選項、kubelet 端點

- **範圍**：只動程式與測試 worker／測試 namespace；現行 agent 未更新。新版二進位編譯到暫存檔後放入測試目錄（上一版保留為 `maccluster-agent.phase2`）。完整說明見 [docs/kubectl-and-kubelet.md](kubectl-and-kubelet.md)。
- **為了能重啟測試 worker 而做的叢集寫入（需要你知道）**：測試 Node 的 kubelet 服務憑證（24 小時）已於 2026-10-19 10:05 GMT 到期，worker 重啟時會因「憑證必須目前有效」而啟動失敗。我為測試 Node `maccluster-nettest-20261018` 建立並以管理憑證核准了一個 `kubernetes.io/kubelet-serving` CSR（`maccluster-nettest-20261018-serving-2`，CN `system:node:maccluster-nettest-20261018`、SAN 為該 Mac 的 IP 與節點名稱，7 天，到期 2026-10-26）；舊憑證與金鑰備份為測試目錄內的 `*.expired-20261019`。沒有修改任何 RBAC，沒有碰其他 Node 或 namespace。
- **實作**：`internal/runtime`（`Commander`、`Attacher`、`UsageReader`；`Exec` 重寫在 `Command` 之上；原生日誌時間索引與每次執行一個檔案；Docker 的 `PreviousStartedAt` 邊界）、`internal/cluster`（`exec.go` 的 exec／attach 提供者與自己寫的 macOS pty、`kubelet_metrics.go` 的 stats 與 metrics、路由與子資源授權：`stats`、`metrics`、其餘為 `proxy`）、`internal/hostinfo`（`CurrentLoad`）。
- **單元測試**（`go vet`、`go test -race -count=1 ./...` 全過）：真的 SPDY 客戶端的 exec（輸出、stderr、結束碼 3、stdin、經 tar 的 cp、TTY 與大小 `30 100`、attach 跟隨輸出）、節點端點的授權子資源與 HTTP 方法、stats 與 metrics 的內容；時間索引的位移對應、`--since`→`--tail`→`--timestamps` 的順序、跨封包行只加一次前綴、一次執行一個檔案與 `--previous`、App 仍顯示所有歷史；`ps` 時間解析、Native 累計 CPU 與記憶體、Docker 的 Engine API 計數；過程中修掉一個 pty 視窗大小與 Close 的資料競爭（改用 SyscallConn）。
- **回歸套件**（30 案例×3 目標＝90 項期望）：全部符合。新增案例 T27 `kubectl exec`（輸出、結束碼 7、stdin）、T28 `kubectl cp` 進出、T29 `kubectl attach`、T30 `--timestamps`／`--since`、T31 `--previous`（容器重啟後）、T32 節點端點（`healthz`、`pods`、`stats/summary`、`metrics/resource` 含該 Pod 的資料）在 Linux、Mac Docker、Mac 原生三者一致。
- **既有網路測試**（副本縮放、日誌 follow、port-forward、readiness 撤除與恢復）重跑全通過。
- **測試腳本修正（非回歸）**：T30 的時間窗口太緊（Linux 對照同樣失敗），改為「等到 new 出現就立刻查」；T32 用了不存在的輔助函式。
- **未驗證／限制**：現行節點的 kubelet 端點未啟用（4.4）；`kubectl top` 需要 metrics-server，叢集沒有，只驗證端點內容；Native 時間戳約 1 秒精度、agent 不在期間的輸出以接管時間標記；Native 的 `exec` 是另起程序；節點 CPU 是以 `ps` 負載積分的估計值；以正常 Node 憑證運行的完整路徑仍未實測（測試 worker 用管理憑證，API server 以其 `system:masters` 身分通過 SubjectAccessReview）。

## 2026-10-19 launchd 原生工作實驗（使用者層級）

暫時 job（前綴 com.maccluster.exp.*）在 gui/501 驗證 bootstrap、print、退出碼、SIGTERM、同名重建、Metal 均通過；已清除。root 身分未測。詳見 docs/launchd-executor-design.md。

## 2026-10-19 launchd 原生後端（測試 worker）

- **範圍：** 只動測試 Node `maccluster-nettest-20261018`（`node-options.json` 加 `nativeBackend: launchd`，二進位換成新版，舊版留作 `maccluster-agent.phase4`）與測試 namespace；現行 agent 未動。
- **發現：** launchd `EnvironmentVariables` 在第一個換行處截斷環境值（回歸套件 T09 的多行 Secret 失敗而發現）；改用 `__launch` 啟動器讀 0600 環境檔後通過。
- **結果：** 回歸套件 89/90（唯一差異為 Linux 目標 T31 偶發，單獨重跑通過）；`kill -9` worker 後 job 接手、重啟次數 0；logs follow、port-forward、副本縮放與 Service 連線、readiness 撤除與恢復通過。單元／整合測試與 `-race` 連跑通過。細節見 docs/launchd-executor-design.md。
- **未測：** root 身分、登出行為、批量啟動；現行 agent 未部署。

## 2026-10-19 環境終端機輸出保存

單元測試 5 項與 App 全部 31 項通過；開發快照實機驗證終端輸出被保存、關閉後可在環境頁日誌區查看，含上一次分隔；自動清除（移除環境、孤兒與 7 天過期、手動清除）。現行 agent 與叢集未動；App 需重新開啟才會載入新版。細節見 docs/terminal-log-design.md。

## 2026-10-19 原生服務自行導向的日誌（測試 worker）

只動測試 worker 與測試 namespace；現行 agent 未動。單元／整合測試（兩個後端）與 `-race` 通過；回歸套件新增 T33，93 項中 92 項符合（T31 Linux 偶發，與本變更無關）。細節與限制見 docs/log-redirect-design.md。

## 2026-10-19 部署到現行 agent（階段 0 到 4、launchd 後端、日誌自動接上）

- **範圍：** 現行 agent 換上新版並重啟（`launchctl kickstart -k`；部署前備份 `.local/backup-agent-pre-phase4`、`.local/backup-agent-pre-logredirect`）；`node-options.json` 未建立，所以 launchd 後端與 kubelet 端點在現行節點**維持關閉**（預設 `process`）。
- **結果：** 重啟後節點 Ready；vLLM Pod 由新 agent 重新啟動，其輸出原本只寫進 `/tmp/agent-kv-vllm-metal-diag/log.txt`，現在 agent 偵測到並自動接上，經 agent 讀取的日誌（App 日誌區同一來源）開頭為 `[maccluster] following output redirected to …/log.txt`，之後是 vLLM 啟動訊息。
- **未確認：** 部署當下 vLLM 仍在載入模型，`/health` 尚未回 200；載入完成後的狀態待使用者通知再確認。

## 2026-10-19 出站代理（測試 worker 與測試 namespace）

動到的叢集資源（皆在測試 namespace `maccluster-net-test-20261018`）：Secret `egress-test`、Pod `egress-gw`、NodePort Service `egress-gw`（31880）；現行 agent 與現行節點未動。結果：Mac 以 curl 經 HTTP／SOCKS5 代理取得叢集 Service 的 200，認證與目的地限制符合預期；回歸套件新增 T34、T35，99 項中 98 項符合（T31 Linux 偶發）。順帶修正：agent 無法為 Linux 編譯（kqueue 與檔案建立時間的程式沒有平台分流），已拆成 darwin／其他平台檔案。細節見 docs/egress-design.md。

## 2026-10-19 agent 正常重啟不停掉工作（測試 worker）

只動測試 worker 與測試 namespace（臨時建立並刪除 Pod `restart-probe`）；現行 agent 未動。SIGTERM 後原生與 Docker 工作存活、重啟後接手（PID 與容器 ID 不變、`postStart` 不重跑、Ready 不閃退、重啟次數 0）；舊版 `kill -9` 後以新版接手；`serve` 模式本機服務存活與 `drain`；launchd 對另起程序群組的子程序在 `kickstart -k` 與 `bootout` 後仍存活。回歸套件 99 項中 98 項符合（Linux T31 偶發）。細節見 docs/agent-restart-design.md。

## 2026-10-19 部署到現行 agent 與 App（agent 重啟不停工作、出站代理程式碼、launchd 後端）

- 部署前備份：`.local/backup-agent-pre-release`（agent）、`.local/backup-agent-pre-release.plist`（LaunchAgent plist）。LaunchAgent plist 加入 `AbandonProcessGroup: true`（App 的「啟動／修復自動啟動」之後也會寫入）。重啟方式：`launchctl bootout` 再 `bootstrap`。
- 這次重啟由舊版 agent 收到 SIGTERM，所以**停止了 vLLM**（舊版行為）；新版啟動後節點 Ready，vLLM 由新 agent 重新啟動。**之後**的 agent 重啟不會再停止工作。
- 現行節點仍未建立 `node-options.json`：launchd 後端、出站代理、kubelet 端點與 serviceNetwork 都維持關閉。

## 2026-10-19 kubelet 端點自簽憑證（測試 worker）

只動測試 worker（`node-options.json` 改為 `kubeletCertMode: self-signed`，原設定備份為 `node-options.json.before-selfsigned`）；現行節點未動。以自簽憑證通過 `kubectl exec/cp/attach/logs/port-forward` 與節點端點全部案例（Linux、Mac Docker、Mac 原生 18/18）；`csr` 模式回歸通過；驗證了 API server 不驗證 kubelet 服務憑證的假設。細節見 docs/production-enable-design.md。

## 2026-10-19 現行節點啟用 kubelet 端點（自簽憑證）

- **動到的東西：** 現行 agent 換成含自簽憑證功能的新版（備份 `.local/backup-agent-pre-kubelet`）；在狀態目錄新建 `node-options.json`（0600，`{"kubeletCertMode": "self-signed"}`）；agent 自動產生 `kubelet-serving.crt/key` 與 `cluster-ca.pem`（0600）。重啟用 `launchctl kickstart -k`。叢集端沒有寫入（節點只更新自己的 `kubeletEndpoint.Port`）。
- **結果：** 重啟後 vLLM 的程序 PID 不變（agent 重啟沒有停掉工作）、節點 Ready、埠 10250 監聽在區網位址；經 API server 的唯讀請求：`/proxy/healthz` 回 `ok`、`/proxy/pods` 列出 vLLM 的 Pod、`kubectl logs deploy/agent-kv-vllm-metal --tail=3` 可讀到日誌。未做 `exec`（依驗證方式的決定只用唯讀請求）。
- **觀察到、與本變更無關：** vLLM 的 API server 程序啟動後有約 27 分鐘沒有輸出、CPU 0%、沒有 engine-core 子程序（等待原因不明）；之後 engine-core 啟動並開始載入模型（日誌在動、CPU 約 60%、記憶體約 9 GB），不是卡死。載入完成前 `/health` 為 000、Pod 0/1。

## 2026-10-19 控制器行為實測（測試 worker）

`scripts/controller-kinds.py`（只動測試 namespace，建立的物件都會刪除）12 個情境全部符合預期：固定埠 Deployment 預設滾動更新停住並說明原因（Recreate 與 maxSurge 0 完成）、動態埠滾動更新、Job 成功／失敗與 backoff／OnFailure／並行／Docker、CronJob、StatefulSet（2 副本）、DaemonSet。同時唯讀查證電源設定：agent 與 App 沒有持有電源斷言，詳見 docs/keep-awake-design.md。

## 2026-10-20 keep-awake（測試 worker）

只動測試 worker 與測試 namespace（臨時將 `native-http`、`nginx` Deployment 縮到 0 再恢復）；現行 agent 與 App 未動。`pmset -g assertions` 驗證斷言的持有、放開、當機清除與 never 模式；`serve` 模式的設定端點與 CLI。細節見 docs/keep-awake-design.md。

## 2026-10-20 部署 keep-awake 到現行 agent 與 App

- 部署前備份 `.local/backup-agent-pre-keepawake`；以 `launchctl kickstart -k` 重啟。重啟前後 vLLM 的程序 PID 不變（約 1 小時 17 分鐘，未被停止），節點 Ready，kubelet 端點仍在 10250，vLLM 的 Pod 1/1。
- 新 agent（PID 92568）持有 `caffeinate -s -w <agent PID>`：`pmset -g assertions` 顯示 `PreventSystemSleep`、`asserting on behalf of Process ID 92568`；`status` 的 `keepAwake` 為 `{mode: when-workloads, held: true}`。（同時另有測試 worker 的 caffeinate，屬於測試 worker。）
- App 已重新開啟載入新版（設定 → 管理程式有「睡眠」區）。實際讓 Mac 睡眠的情境沒有測。

## 2026-10-20 滾動更新實測（測試 worker）

新增 `scripts/rollout-checks.py`（只動測試 namespace，建立的 Deployment 都刪除）：8 個情境全部符合預期。**發現並修正：** 沒有探針且啟動就崩潰的更新，舊版 agent 會讓新 Pod 在每次重啟的瞬間回報 Ready，Deployment 因而換掉舊副本（Linux 不會，已用同一個更新在 Linux 節點對照：全程 3 個 Ready）；現在程式需執行滿約 2 秒才 Ready，單元測試與實測都通過。回歸套件 T01 因此改為等待 Ready。修正尚未部署到現行 agent。

## 2026-10-20 Docker 卷宗、結束後清理與 Docker 登入（測試 worker）

- **卷宗**：`node-options.json` 加 `allowedHostPaths`，agent 換新版；回歸套件新增 T40–T48，全部符合預期（Linux、Mac Docker 與 Mac 原生拒絕）。實測抓到一個真的錯誤：agent 以相對的 `--state-dir` 啟動時卷宗路徑是相對的，Docker 拒絕；已改為絕對路徑。細節見 docs/volumes.md。
- **清理**：Pod 完成（Never）後卷宗被清、重啟中的 Pod 保留、不相關的目錄不被清；先前失敗測試留下的孤兒卷宗目錄也由清理器清掉。
- **Docker 登入**：設定頁新增「Docker 登入」，agent 新增 `docker-login`／`docker-logout`／`docker-logins`；對本機有密碼驗證的 registry 實測（已清除）。見 docs/docker-registry-login.md。鑰匙圈寫入未在 App 內實測。
- 現行 agent 與 App 尚未部署這些變更。

## 2026-10-20 部署卷宗、清理器與 Docker 登入到現行 agent 與 App

- 部署前備份 `.local/backup-agent-pre-volumes`；`launchctl kickstart -k` 重啟。重啟前後 vLLM 的程序 PID 不變（約 12 小時 44 分鐘，未被重啟），節點 Ready，kubelet 端點仍在 10250，vLLM Pod 1/1、`/health` 200，keep-awake 仍持有。
- 現行節點沒有 `allowedHostPaths`（hostPath 全部拒絕）；`volumes/` 目錄在第一個帶卷宗的 Pod 出現時才建立。`docker-logins` 回報目前沒有登入。App 已重開載入新版（設定 → Docker 登入）。
- 鑰匙圈寫入與實際拉取私有映像尚未在 App 內實測，需要在桌面階段登入一個映像庫確認。

## 2026-10-20 ServiceAccount token（3c、3d）與真實 Node 身分（測試 worker）

- **叢集寫入（都只影響測試節點與測試 namespace）：** 一個 CSR `nettest-node-client-20261020`（signer `kube-apiserver-client-kubelet`，7 天，已核准）為測試節點簽發 `system:node` client 憑證；測試 namespace 內建立並刪除 ServiceAccount／Role／RoleBinding `tok-reader`、測試 Pod。為此把 `native-http`、`nginx` Deployment 縮到 0（Service 網路在 Node 身分下失敗，見下）。
- **探測（Node 身分的模擬請求）：** 為自己節點上的 Pod 取得綁定 token → 允許；為別的節點的 Pod → 拒絕；修改 Pod annotation → 拒絕（所以現行 Service 網路設計在正式節點會壞，需改用 `pods/status` condition，見 docs/archive/service-network-and-token-plan.md）。
- **結果：** 測試 worker 以 Node 身分運行，整份回歸套件 123/123；新案例 T49、T50、T51 對 Linux、Mac Docker、Mac 原生全部符合預期（token 呼叫 API 回 200／403、Pod 刪除後 401、未設定時不掛載且有 Warning）；單元測試涵蓋綁定、輪替、失敗處理與政策。T52（輪替，約 10 分鐘）結果見下一則。現行 agent 尚未部署。
- **T52（輪替）：** `expirationSeconds: 600` 的 token，Linux 與 Mac Docker 都在測試期間（約 9 分鐘內）被換新；Mac 原生對該案例預期 UNSUPPORTED，符合。

## 2026-10-20 Service 網路：公布埠改用 pods/status condition（測試 worker，Node 身分）

- **起因：** 以 Node 身分運行時，agent 修改 Pod annotation 被拒絕（`cannot patch resource "pods"`），所以現行 Service 網路設計在正式節點會壞。**改動：** 入口埠對應改寫在 Pod 狀態的 condition `maccluster.example.com/service-ports`；controller 讀 condition（舊 annotation 為後備）。
- **叢集寫入（測試 namespace）：** 重建測試用的 `network-controller` Pod（Linux 節點，新版二進位，以複製檔案方式部署，與先前相同）；把 `native-http`、`nginx` Deployment 縮放（0 再 1，以及檢查腳本內的縮放）。
- **結果（Node 身分）：** 單元測試（condition 的寫入與讀取、False 條件、損毀訊息、舊 annotation 後備、agent 不呼叫 API）通過；`check-mac-only.py`（副本 0／1 的 Ready Pod 與 EndpointSlice、唯一埠、Linux→Service→Mac TCP）、`readiness-check.py`（撤除與恢復）、`lease-check-mac-only.py`（lease 過期撤除、恢復後 Service 名稱可達）全部通過；節點沒有寫入任何 Pod annotation。現行 agent 尚未部署這個改動，現行節點仍未啟用 Service 網路。

## 2026-10-20 Service 網路入口埠的來源白名單（測試 worker，Node 身分）

只動測試 worker（`node-options.json` 的 `serviceNetworkAllowFrom` 輪流設為四種值，最後留 `198.51.100.0/24`）；現行節點未動。量測：Linux Pod 經 Service 連 Mac 入口埠時，Mac 看到的來源是 Pod 所在節點的 IP。結果：不設／`198.51.100.0/24`／只允許 `198.51.100.208`／`192.0.2.0/24` 四種設定下，來自 `198.51.100.208`、`198.51.100.81` 兩個節點與 Mac 自己的連線都符合預期（通過或被重設），拒絕有日誌；`check-mac-only.py`、`readiness-check.py`、`lease-check-mac-only.py` 在白名單下全部通過。細節見 docs/service-network.md。

## 2026-10-20 Service controller 正式化：離線部分

不需要叢集寫入的部分：leader election、健康端點、manifest 產生器、多架構 Dockerfile 與建置腳本，與其單元測試（`-race`）；本機建置兩個平台的映像並載入本機 Docker 啟動驗證（測試映像已移除）。**沒有**推送映像、沒有任何叢集變更（`network-controller-manifests` 只印出 YAML）。細節見 docs/service-controller-plan.md。

## 2026-10-20 kubectl 直連失敗：確認 macOS 本機政策丟棄

- 同一執行環境與目標 control plane TCP 6443：系統路由有明確 en7 路徑，Apple nc 連線成功、curl 使用既有 CA 驗證後 `/version` 回 HTTP 200、Apple Python socket 成功；Docker 隨附的 kubectl 立即回 `no route to host`。shell 無 HTTP/HTTPS/ALL/NO_PROXY 環境變數，admin kubeconfig 的 server 正確且沒有 proxy-url。診斷已在 sandbox 外執行，以排除工具 sandbox 的路由 socket 限制。
- 直接證據：同次 kubectl PID 的 kernel log 為 `tcp drop outgoing ... reason: NECP`，隨後 `error: 65`，`SYN in/out: 0/0`、`pkts in/out: 0/0`。因此本次失敗是本機網路政策在建立連線時拒絕，不是 API server 不可達或 Kubernetes 憑證／RBAC 拒絕。
- 唯讀 NetworkExtension 規則：`com.openai.codex` 有已啟用的 networkprivacy path controller，規則 `MulticastPreferenceSet=true`、`DenyMulticast=true`。此為內部欄位，不單靠欄位名稱判定所有網路政策；與 UI 和 kernel log 合併判讀。
- Computer Use 實際打開「系統設定 → 隱私權與安全性 → 區域網路」：ChatGPT 顯示 off；Docker、MacCluster、maccluster-agent 顯示 on。Claude 沒有出現在當次 UI 清單，其內部規則 preferenceSet=false，不能據此宣稱使用者已明確拒絕 Claude。
- 結論：已確認目前 Codex 工具環境的 kubectl 流量被 macOS NECP 拒絕，區域網路授權是主要檢查點。未切換權限、未重置隱私設定、未修改系統路由或叢集，也未重新打包程式。仍待使用者允許對應 App 的區域網路存取後，在同一環境重試，以完成權限切換的因果驗證；Claude 的啟動環境需分別確認。
- Apple 說明：<https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy>、<https://support.apple.com/zh-tw/guide/mac-help/mchla4f49138/mac>。

### 同日：使用者開啟區域網路權限後重測

- 已確認 `com.openai.codex` 規則由 `DenyMulticast=true` 變為 false，`MulticastPreferenceSet=true`；這證明使用者切換確實更新了該 App 的規則。
- 相同 Codex 執行環境、相同 admin kubeconfig 直連 `/version` 與 `get nodes -o wide` 仍失敗；同時 curl 經 CA 驗證仍 HTTP 200。
- 新的 kubectl PID 對應 kernel log 仍有 `reason: NECP`、`error: 65`、SYN 0/0。不能宣稱權限切換已解決問題，也不能再只歸因於主 App 開關關閉。尚待重新啟動 Codex 後重測，及從使用者的 Terminal 執行同一 kubectl 作為啟動環境對照；helper/CLI 規則 preferenceSet=false 不等於已證實是哪一條規則拒絕。

## 2026-10-20 運行日誌與側邊欄

- 運行日誌空白狀態在剩餘內容區域置中；新增持久化 Kubernetes 加入、退出、節點移除、Pod 指派／狀態／移除事件。最近 1000 筆保存在權限 0600 的 events.json，退出叢集後保留；更新前的歷史事件無法回補。
- 等級以 ANSI 對應色顯示：INFO 綠、WARNING 黃、ERROR 紅，保留文字辨識。實際畫面驗證三筆 INFO 事件；warning/error 對應由測試驗證，未在現行服務注入故障。
- Computer Use 驗證叢集及 Compose 列雙擊展開／收合、官方白色 Kubernetes 圖示，以及日誌空白與有資料時的排版。退出完成提示改為「已離開叢集。」。
- 驗證：Go `go test -race ./cmd/maccluster-agent ./internal/cluster` 通過；Swift 39 項測試通過；`scripts/build.sh` release 打包與簽章驗證通過。
- 已更新本機 App 與背景 agent。更新前後現行 vLLM 工作的 UID、PID、Running 狀態與零重啟次數一致；沒有新增、刪除或修改叢集工作。

## 2026-10-20 使用者要求：vLLM Deployment 更名與重啟

- 範圍僅 `maccluster-test` 的 `agent-kv-vllm-metal` 與已停用（replicas=0）的 `vllm-metal`；透過 control plane SSH 執行 kubectl。
- 兩份原始 Deployment JSON 以受限權限備份到 control plane 的 `~/.local/state/maccluster-backups/rename-vllm-<timestamp>/`。舊工作縮至 0 並等待 Pod 刪除後，將現行工作設定移至 `vllm-metal`，沿用目標既有 selector，避免同時占用固定埠 8100。
- 初次排程受既有 `node.kubernetes.io/network-unavailable:NoSchedule` 阻擋；僅為此 native 工作加入相應 toleration，未移除 Node taint。容器設定（模型、命令、環境、探針等）與 nodeSelector 比對備份一致。
- 結果：`vllm-metal-<replicaset>-<suffix>` 在指定 Mac 節點達到 1/1 Ready、Running、0 restarts；Metal GPU 模型載入完成。`/health`、`/v1/models` 與一次最小 `/v1/chat/completions` 均成功（HTTP 200，模型 `gemma-4-e4b`）。驗證後刪除副本為 0 的舊 Deployment；namespace 保留，沒有操作其他工作。

## 2026-10-20 CLI 操作整合測試

- 使用 `dist/maccluster-agent` 與目前背景 agent，測試腳本／結果保存在未納管的 `.local/cli-smoke/check.py`、`.local/cli-smoke/result.json`。全部 21 個檢查通過。
- 建立臨時 Python venv；`env add/list --json/shell/remove` 成功，shell 輸出啟動設定 JSON，remove 後環境目錄仍存在。
- 使用獨立名稱建立 native HTTP 與預設 executor 的 nginx Docker 本機服務；兩者 HTTP 200、status Running、logs 可讀，隨後以 CLI delete 清除。
- 建立獨立外部 Docker 測試容器；inventory、docker-inspect、docker-stats、docker-logs、docker-control start/stop 全部成功，最後移除測試容器。
- join 無效指令、configure/leave 不存在的 profile 均以非零退出碼拒絕。本次未實測成功加入／退出遠端叢集，也未測 drain、registry 登入或變更 settings。無效 join 可能在運行事件中留下測試失敗紀錄，這不表示現有叢集失敗。
- 清理所有本次建立的工作、環境登記、外部容器與臨時 venv。前後比對現有工作的 UID、PID、restarts、phase 完全一致；現有 vLLM 未重啟。


## 2026-10-20 單一建置入口

- `scripts/build-app.sh` 已整併至 `scripts/build.sh`，上方歷史紀錄中的舊命令僅代表當時測試。現在使用 `build.sh` 建置完整 CLI 與 App。
- 新增 `--check`、`--no-repair` 與英文 help。自動選用已安裝的相容 Xcode（限本程序）、從官方發布清單下載並驗證 SHA256 的 Go 工具鏈、建立快取；不自動接受 Apple 授權或執行 sudo。
- 本機實測 Go 工具鏈缺少時，自動下載安裝至 `.tools/go`，`--check` 通過。
- 模擬無效 `DEVELOPER_DIR` 時成功改用已安裝的相容 Xcode；`--check --no-repair` 亦通過。
- 使用者確認 `app/` 並非刻意刪除後，從 Git 恢復原始碼與資源。完整 `./scripts/build.sh` 成功產出 CLI 與 `dist/MacCluster.app`，release 編譯與 ad-hoc 簽章驗證通過；沒有重啟 App、背景服務或操作叢集。
- `python3 scripts/test-build.py` 通過 help、無效參數／Go override、缺少原始碼、停用修復、下載校驗失敗與暫存清理檢查；`zsh -n scripts/build.sh` 通過。去識別化掃描通過 207 個文字檔，截圖仍依使用者要求保留原圖。

## 2026-10-22 Docker 未啟動時的啟動與恢復

- 唯讀檢查發現 app 的 `launchctl kickstart` 已等待超過四分鐘，沒有 agent 程序；UI 的 busy 因啟動流程尚未結束而持續為 true。未終止既有 app／命令、未重啟 agent 或操作叢集。
- 後續唯讀 `launchctl print` 確認載入的 program 仍為舊 AppTranslocation 暫存路徑，state 為 spawn scheduled、最後退出碼為 78（EX_CONFIG）。修正為比對 launchd 已載入的 arguments：與目前 binary／state-dir 不一致時先 bootout 再 bootstrap；一致才 kickstart。參數格式無法辨識時回報錯誤，不盲目重載。相關比對測試涵蓋含空白的路徑、舊暫存路徑、state-dir 改變與未知格式；本次未實際 bootout／bootstrap 既有服務。
- `launchctl print` 加入 3 秒逾時，kickstart／bootstrap／bootout 加入 10 秒逾時；print 逾時不當作「服務未載入」再送 bootstrap。命令逾時先 SIGTERM，300 ms 後仍存活才 SIGKILL，並回報明確逾時錯誤。
- status、Docker inventory、執行環境三項查詢獨立進行、各自更新 UI；Docker 清單不因 agent status 卡住而延後顯示。重疊的 refresh 共用同一 Task 結果，避免 initialize 將進行中的查詢誤判成 agent 未啟動。
- 啟動失敗使用獨立 startupError；啟動流程結束會解除 busy。Docker 清單成功後清除 inventoryError；agent 狀態恢復後清除 startupError，不會誤清除其他操作的 actionError。
- `AgentRecoveryTests` 以注入的回應驗證同一 app session 內 Docker 不可用 → Docker 可用但 agent 不可用 → 兩者可用，確認清單自動查詢可恢復、錯誤按各自狀態解除，且不會再次啟動 agent。慢 status／重疊 refresh 測試亦通過。模擬後端停用終端日誌清理，不對實際 agent 或狀態目錄執行寫入。
- 真實子程序測試使用忽略 SIGTERM 的 `/bin/sleep`，確認逾時後能強制結束並接續成功執行下一個命令。測試程序已退出。
- Swift 回歸命令：`swift test --filter 'AgentRecoveryTests|StatusDecodingTests|SidebarSplitViewTests'`，共 11 項通過，涵蓋啟動恢復、狀態解碼與先前分欄／長日誌行為。
- 目前本機唯讀 inventory 查詢仍回報不可用；沒有為測試開關 Docker Desktop，也未執行真實 off → on 的端到端驗證。該恢復路徑目前的證據來自模擬後端。

### 同日：代理已就緒但畫面仍顯示等待

- 唯讀檢查確認 launchd 的 agent 為 running，使用目前 app 的正確 binary；以同一 state-dir 查詢 status 回覆 Ready。本次問題是 UI 的等待提示沒有隨後續成功刷新而更新，不是 agent 仍未啟動。
- 啟動提示改為 starting／waiting／ready 的獨立型別狀態；後續 status 成功會將 starting／waiting 更新為 ready。一般操作的 notice 保留獨立，避免代理恢復時覆蓋建立服務等操作結果。
- 啟動／重啟套用相同提示狀態；失敗或停止時清除啟動提示。若較新的 status 已回覆成功，較早一次失敗查詢的收尾不會將提示降回 waiting。
- 新增延後就緒的回歸測試，驗證 waiting → ready 與保留其他操作結果。`AgentRecoveryTests|StatusDecodingTests` 共 9 項通過；模擬後端不執行實際 agent 啟停或終端日誌清理。

### 同日：活動監視器終止代理的行為調查

- 唯讀檢查目前 LaunchAgent：`KeepAlive=true`、`RunAtLoad=true`、`AbandonProcessGroup=true`，launchd 顯示 running、runs=8。runs 是啟動次數，不代表使用者終止次數。終止 daemon 後出現新的 PID，是 KeepAlive 重新啟動服務；app 的安裝程式也明確寫入此設定。
- 以目前 app 隨附的 agent binary，在獨立 `/tmp` state-dir、無工作／無叢集設定、假 Docker（僅退出 0）、keepAwake=never 的環境量測。沒有終止或重啟既有代理、沒有操作實際 Docker／control plane。腳本與結果保存在未納管的 `.local/agent-stop-check/`。
- 閒置 SIGTERM：0.004 秒、exit 0；保留未完成 HTTP request body 後 SIGTERM：5.020 秒、exit 1，期間另外兩次 SIGTERM 未縮短等待；閒置 SIGKILL：0.004 秒、exit -9。測試子程序皆已回收，未註冊 launchd job。
- 程式的 HTTP graceful shutdown 等待上限為 5 秒，之後另外給 server cleanup 60 秒 context；部分互斥鎖／Release 路徑不受 context 截止時間限制，不能視為嚴格的總退出上限。HTTP request context 未連接到 signal context，且 signal handler 直到函式返回才解除，能解釋上述未完成請求等待與重複 SIGTERM 無效。尚未在使用者當時的真實負載重現更長延遲。
- 若要停止且避免當下自動重生，現有 UI 的「設定 → 管理程式 → 停止 → 只停止管理程式」使用 launchctl bootout 卸載 job；保留工作及 LaunchAgent 設定，下次登入仍會啟動。本次僅調查，未變更 KeepAlive 或退出邏輯。

### 同日：純 CLI 背景服務管理

- 新增 `maccluster-agent service install|start|stop|restart|status`，與 GUI 共用目前使用者的 `com.maccluster.agent`。install 以目前 executable 的固定路徑產生權限 0600 的 LaunchAgent；已載入時拒絕改寫，須先 stop。其餘操作使用已登記的 plist，不覆寫執行檔或 state-dir。
- stop 使用 bootout 卸載服務，保留工作、避免 KeepAlive 立即重生；plist 保留，下次登入仍啟動。restart 為 bootout 後 bootstrap；start 對已載入服務 kickstart，未載入才 bootstrap。status 唯讀顯示 launchd 資訊；工作狀態仍由原有 status 查詢。前景 serve 的 Ctrl-C／kill 行為未變更。
- 所有 launchctl 呼叫有 10 秒逾時；僅確認 exit 113（service not found）才視為未載入，逾時／權限等錯誤不觸發安裝或 bootstrap。唯讀實測不存在的專用服務名稱確實回傳 113。
- `go test ./cmd/maccluster-agent -run TestService -count=1` 通過：涵蓋啟停／重啟命令順序、job 目標、逾時不繼續變更、已載入設定保護、XML 路徑跳脫、plist 權限及 plutil 格式驗證。啟停使用注入的 runner，未對實際服務執行寫入。
- Go build 成功；更新獨立的 `dist/maccluster-agent`，舊 binary 保存在未納管的 `.local/before-cli-service/`。新 CLI 的 help 與實際 service status 成功，現有服務仍為原 PID 97283、runs=8；未重啟既有服務、未替換 app 隨附的 agent，也未操作工作或叢集。

### 同日：SIGKILL 抑制自動重啟

- 新版 LaunchAgent 以固定 shell 監督程序等待 agent，agent 被 SIGKILL 結束時（wait status 137）回傳 0，配合 `KeepAlive.SuccessfulExit=false` 保持停止；一般退出及 SIGTERM 仍觸發重新啟動。監督程序收到 TERM／INT 時轉交 TERM 並回收 agent。GUI 改由 `service config` 取得相同 plist，該命令沒有檔案或 launchctl 寫入。
- `go test ./cmd/maccluster-agent -run 'TestService|TestAgentSupervisor' -count=1` 通過，涵蓋實際子程序 SIGKILL／SIGTERM、監督程序轉交終止、一般退出碼 0／1／2、config 無副作用與原有 service 測試。
- 以專用 `com.maccluster.restart-test.<test-pid>` job 實測真正 launchd，子程序僅 `/bin/sleep`，不啟動實際代理／Docker／叢集。SIGTERM 後出現新 PID；SIGKILL 後 supervisor exit=0、觀察 3 秒（測試 ThrottleInterval=1）無新 PID、runs 不增加；kickstart 可恢復，bootout 後子程序已回收。測試亦確認 launchctl 的 arguments 輸出可與 GUI 預期陣列完整比對。所有專用 job／子程序已清理；腳本及結果在未納管的 `.local/test-agent-restart.py`、`.local/agent-restart-result.json`。
- Swift `AgentRecoveryTests|StatusDecodingTests` 共 9 項通過，release build 成功。更新獨立 CLI、app 隨附 agent 與 GUI；`codesign --verify --deep --strict` 通過。成品的 CLI／bundled agent 輸出相同政策；舊成品備份在 `.local/before-sigkill-policy/`。
- 沒有對既有 job 執行 bootout／bootstrap，也未終止既有工作。既有 status 唯讀查詢 8 秒逾時，因此未推定它沒有工作。已載入的舊設定仍須由使用者在適當時間執行 `service stop` → `service install` → `service start`，或明確用新版 App 啟動／修復。僅更新成品不能改變現有 launchd 政策。

### 同日：使用者授權套用並實測目前服務

- 使用者明確要求套用新版設定、實際 SIGKILL 並確認工作不受影響。操作前 status 成功：Ready、clusters=0、localWorkloads=0；實際 Docker socket 不存在，Docker 查詢不可用。因此本次沒有既有受管工作可比對，也沒有宣稱驗證 Docker 工作存活。
- 建立專用本機 native Python HTTP 測試服務，僅綁 127.0.0.1、自動分配埠，回覆專用 nonce。建立後啟動 HTTP 取樣，記錄 UID、PID、Port、StartedAt、Restarts；不變更或部署 control plane 資源。
- 備份現有 plist 後，透過 app 隨附的新 CLI 執行 `service stop` → `service install --state-dir <原目錄>` → `service start`。保留原本 binary／state-dir 路徑；原無條件 KeepAlive 改為 SuccessfulExit=false 與監督程序。一般停止及套用後測試工作維持相同 PID、零重啟並被重新接手。
- 核對 supervisor／agent 的父子關係及 agent 程序名稱後，僅對實際 agent PID 送一次 SIGKILL。連續觀察 15.18 秒，launchd runs 不增加、job 沒有 PID、supervisor exit 0，沒有自動重生；測試工作 PID 身分不變，HTTP 持續成功。
- 隨後 `service start` 恢復代理。測試腳本第一次在 kickstart 返回後立即讀 socket，遇到 connection refused；這是尚未就緒的取樣時序，後續等待成功，非永久啟動失敗。代理 Ready，接手相同 UID／PID／Port／StartedAt 的工作，Restarts=0；腳本已補上就緒重試，沒有重做已通過的 SIGKILL 測試。
- 整段已取樣的 HTTP 檢查合計 88 次、失敗 0。最後只刪除本次測試服務，確認其 PID 已退出；status 回到 Ready、clusters=0、localWorkloads=0，與原工作基線相符。新版 plist 持續生效，目前代理已恢復運行。
- 受限權限的舊 plist／狀態快照、測試腳本、HTTP 取樣與結果保存在未納管的 `.local/live-sigkill-test/`。目前無待清理測試工作；未啟動 Docker 或操作叢集。

### 同日：加入提示停留與健康頁端點文字

- 唯讀 status 顯示加入已失敗：Node 名稱未以 `maccluster-` 開頭。加入視窗的「預設」原先填入裸 hostname；API 先回覆 202，Bootstrap 才檢查名稱，UI 的加入 notice 又不隨後續 phase 更新。代理 netcheck 實測可直接連到控制平面；本次失敗發生在名稱驗證，尚未進入 CA／token 驗證，不把它歸因於區域網路權限。
- UI 改為允許 Node 名稱留空，由代理產生具安裝識別的合法名稱；「預設」還原空白，自訂名稱在送出前驗證。CLI／API 共用 Bootstrap 名稱驗證，非空且不合法的名稱改為同步 HTTP 400，不保存 profile 或啟動 Bootstrap。
- UI 追蹤加入回應的 profile ID，後續 refresh 顯示實際進度、Ready 或錯誤；即使失敗發生在第一次 refresh 前，也不會停留在「已送出」。錯誤後恢復 Ready 會清除加入錯誤，其他操作的 notice 保留。已接受的加入會選取相應叢集。
- 健康頁將已知叢集端點的 URL／host:port／host 顯示為「控制平面」，包含列標題、說明、提示與使用相同文字的無障礙標籤。App 的連線測試仍使用真實目標；CLI 診斷保留原始位址。保留既有權限檢查功能，沒有硬編碼使用者內網 IP。
- 確認此次失敗 profile 的目錄為空、沒有 node.kubeconfig 或工作後，備份 profiles.json，僅透過 leave API 清除此本機空紀錄，解除同端點重試阻擋。沒有遠端 Node／憑證可供該清理操作使用，未操作控制平面。備份及本機驗證結果在未納管且權限受限的 `.local/join-fix/`。
- Swift 首輪 `JoinProgressTests|HealthCheckTests|AgentRecoveryTests` 共 18 項通過；增加名稱尾端換行拒絕、失敗後恢復與共用服務啟動路徑後，相關套件重跑通過。Go `TestJoinInvalid|TestService|TestAgentSupervisor` 通過；涵蓋不合法名稱同步拒絕、零 profile／目錄，以及先前 SIGKILL 政策。
- 套用時觀察到 bootout 返回後，舊 job 仍可能短暫可見，立即 bootstrap 回傳 5；先恢復代理，再補上有界重試。僅針對 bootstrap exit 5 且 lookup 成功或確認缺少 job 時，以 500 ms 間隔最多重試 10 次；lookup 權限／逾時錯誤不重試，最終錯誤仍保留。GUI 共用 CLI 的啟動路徑。測試包含尚可見／不可見的過渡 job 與 lookup 逾時；新 CLI 的實際 service restart 最後成功，代理恢復 Ready。
- 更新獨立 CLI、app 隨附 agent 與 GUI；release build 與 deep strict 簽章驗證成功。已重啟無工作的代理載入修正；實際以假 token／文件保留位址送出不合法名稱，立即拒絕且沒有建立 profile。最終 Ready、clusters=0、localWorkloads=0，保留 SuccessfulExit=false 的 SIGKILL 政策。沒有重新送出使用者的真實 join／bootstrap 請求，也沒有宣稱這次已完成叢集加入；GUI 需重開後使用新版表單重試。

### 同日：正式建置固定清除 UI 快取，保留 Go 快取

- `scripts/build.sh` 每次正式建置都移除 `app/.build`、先前診斷用的 `.local/swift-build` 與 `.local/sidebar-build`；不再只於專案搬移時清理。保留 `.tools/gomod`、`.tools/gocache`、`.tools/gopath` 與 Go 工具鏈，以及其他 `.local` 資料。
- Swift 的 scratch／套件 cache／Clang 及 Swift module cache 明確集中在重新建立的 `app/.build`，停用共用 dependency cache 與 manifest cache。編譯與 show-bin-path 使用同一組參數，避免打包其他 scratch 目錄中的 UI。`--check` 在清理前結束，不刪 UI 快取。
- `python3 scripts/test-build.py` 通過：在離線暫存 fixture 連續兩次執行清理，既有同路徑 UI 快取皆刪除；Go 快取、工具鏈與無關 `.local` sentinel 保留；`--check` 保留 UI sentinel，Swift scratch／cache／manifest 參數均符合設定。原有 help、參數／toolchain 檢查與 checksum 拒絕／清理測試仍通過。`zsh -n scripts/build.sh` 通過。
- 真實 `./scripts/build.sh --check --no-repair` 保留三處 UI sentinel；接著 `./scripts/build.sh --no-repair` 完整成功，重新取得 SwiftTerm／ArgumentParser 並從頭 release 編譯，成品簽章驗證通過。三處舊 UI sentinel 消失、兩處舊診斷 scratch 目錄已移除；既有 Go 快取／工具鏈目錄 inode 保留，抽查 Go module 的 go.mod inode 與 SHA256 不變。打包的 UI UUID 與此次 `app/.build` 新產物一致。
- 實測結果在未納管的 `.local/build-cache-before.json`、`.local/build-cache-result.json`。未重新啟動 App／代理或操作叢集；已執行中的 App 仍須重開，才會載入新的 UI binary。

### 同日：強制關閉後手動啟動被 No such process 阻擋

- 使用者截圖顯示 socket connection refused，重新啟動又回報 Boot-out failed: 3。唯讀確認當下 job lookup=113（job 已不存在），plist 還指向 AppTranslocation 暫存路徑。先備份 plist，以專案內 app 隨附 CLI、原 state-dir 重新安裝並啟動，status 恢復 Ready、clusters=0、localWorkloads=0。
- 根因是重新啟動先無條件 bootout，將「已不存在」當成致命錯誤，未繼續安裝／啟動。CLI stop／restart 現在僅於 bootout exit 3／113 且後續 print 確認 113 時視為已停止；仍可見的 job、權限／逾時錯誤不會被忽略。成功 bootout 後會有界等待確認 job 移除，避免過渡期內錯誤宣稱完成。
- GUI 的停止及變更已載入設定時的卸載共用 CLI stop；不把 --state-dir 傳給僅操作登入 job 的 stop 指令。重新啟動繼續使用相同啟動／修復路徑。移除原停止操作的 try?，真正失敗時保留錯誤，不宣稱已停止。
- Go `TestService|TestAgentSupervisor` 通過，新增已卸載時 stop／restart、停止後等待 job 消失、仍載入／無法查詢時不忽略錯誤。Swift `AgentRecoveryTests|JoinProgressTests` 共 13 項通過，含重新啟動會呼叫 start、真正 stop 失敗不繼續也不誤報成功。初次 Swift 測試遇到舊掛載路徑 PCM 混用，清除 app/.build 後成功；正式 build.sh 另完整清理 UI 快取並重建成功，最終成品 deep strict 簽章驗證通過。
- 在無工作基線下實際對代理 SIGKILL：代理保持停止，手動 service restart 恢復 Ready。再 stop 卸載、重複 stop（成功），最後從 job 缺少狀態 restart 也恢復 Ready。早期測試於 bootout 後立刻檢查仍可見 job，因而補上等待確認；最後完整流程通過。未操作控制平面或停止其他工作，SuccessfulExit=false 政策保留。
- 舊 plist、測試日誌／腳本與最終結果在未納管的 `.local/forcekill-recovery/`。目前 agent 為 Ready、clusters=0、localWorkloads=0。GUI 需結束舊的暫存 App，再開專案內 dist/MacCluster.app 才會使用新版停止／重啟邏輯。

### 同日：啟動按鈕閃一下、未顯示啟動進度

- Computer Use 讀取實際管理頁，確認錯誤為 `flag provided but not defined: -state-dir`、`Usage of service start`。前次將最後的 bootstrap 改為 CLI service start 時，沿用會一律附上 state-dir 的 API command helper；service start 不接受該參數，因此在解析命令時即退出，沒有成功建立常駐代理。當時 job lookup=113、沒有 agent 程序，不能把短暫 CLI 程序當成已啟動後崩潰的 daemon。
- commandArguments 現在只對 service install／config 附加 state-dir；start／stop／restart／status 不附加，原 API 命令仍保留原目錄參數。
- 加入獨立 serviceStarting 狀態。開始／重新啟動期間，管理頁顯示 ProgressView、「正在啟動管理程式…」與等待說明，按鈕文字改為「正在啟動…」。安裝／kickstart 返回後，以每次最多 2 秒的狀態查詢等待連續兩次回應，間隔 300 ms；確認前維持 busy，不把單次短暫回應當成就緒。等待窗口為 12 秒，單次請求可能跨越截止點；失敗後解除 busy 並在同一設定頁顯示 startupError。Docker／環境清單更新不延長啟動 busy。
- Swift `AgentRecoveryTests|JoinProgressTests` 共 16 項通過，包含 service 參數分流、延後就緒期間進度保持、只回應一次後退出不判定 Ready、失敗解除 busy，以及先前啟動／加入流程回歸。正式 build.sh 清除 UI 快取後完整建置、簽章驗證成功。
- 已用 Computer Use 結束舊 App、開啟新成品；確認無受管工作後，實際按「停止 → 只停止管理程式」，看到「未運行」，再按「啟動／修復自動啟動」，看到「運行中」且參數錯誤消失、按鈕恢復可用。此次啟動很快，AX 取樣時已完成，未直接捕捉轉圈的中間畫面；進度中間狀態由測試驗證，不宣稱現場錄到了該動畫。
- 再唯讀追蹤真正 serve PID 30.79 秒：PID 不變、launchd runs 不增加、15 次 Unix API 查詢皆 Ready，沒有觀察到啟動後閃退。SuccessfulExit=false 政策保留，未變更 AppTranslocation／quarantine 或區域網路權限。新版 App 與代理已留在運行狀態，未操作受管工作或外部容器。
- 測試輸出、建置日誌及觀察結果在未納管的 `.local/startup-progress-tests.log`、`.local/startup-ui-fix/`。

## 2026-10-22 區域網路權限與健康檢查

- **現象：** macOS 27 的這台 Mac 上，agent 連控制平面回報 `no route to host`，但同一時間 `nc`、`ping` 都通；同一台上的 Python socket 與 Go 程式也同樣得到錯誤碼 65。先前 `docs/validation.md` 記錄的「Go／kubectl 直連失敗、curl 與 SSH 可達」是同一現象。這與 macOS 的「區域網路」隱私設定吻合：Apple 簽署的工具（`nc`、`ping`、`ssh`、`curl`）不受限，其他程式需要授權。**這是依行為做的推論；未取得系統的明確拒絕紀錄，也未驗證「授權後 agent 能連」。**
- **另一個失敗原因：** `node.kubeconfig` 的 `proxy-url` 指向本機 SOCKS 隧道（127.0.0.1:18080），該隧道沒有運作時連線被拒；agent 的節點連線迴圈出錯一次就停止、不重試。已備份並移除這個設定（`node.kubeconfig*.before-direct`），改直連後仍受上述權限問題影響。
- **實作（尚未部署到現行 agent）：** agent 新增 `GET /v1/netcheck` 與 `maccluster-agent netcheck`，在 agent 程序內檢查：直連控制平面（`No route to host` 且 ping 可通時指向區域網路權限）、kubeconfig 的代理是否可連、節點憑證到期日。App 設定新增「健康檢查」分頁（agent、App 自身的區域網路權限、agent 的網路檢查、Docker），並在第一次連到區域網路叢集前先顯示說明再觸發系統的授權提示。檢查只能依連線行為推測，畫面上以「很可能」表述。
- **測試：** Go 單元測試以注入的探測函式涵蓋各種錯誤分類；Swift 測試涵蓋分類、位址判斷與解碼，並以真實的本機連線測試 `LocalNetworkProbe.connect`。以真實的 `runNetChecks` 在本機跑一次，輸出與上述現象相符（連線失敗並指向區域網路權限、無代理、憑證 2026-10-27 到期）。
- **發現：** 現行節點憑證將於 2026-10-27 到期；節點目前無法連上控制平面，所以無法自動更新。

### 2026-10-22 部署健康檢查到現行 agent 與 App

- 備份現行 agent：`.local/backup-agent-pre-netcheck`。以 `./scripts/build.sh` 建置，`launchctl kickstart -k` 重啟 agent（重啟前沒有原生程序在執行，所以「工作不被停掉」這點這次沒有實際被檢驗）；App 關閉後重開。
- 第一次重啟後 launchd 顯示上一個行程的結束原因為 `OS_REASON_CODESIGNING`（新二進位被重新簽署後的第一次啟動被系統終止），隨後自動重啟。第一次連線仍回報 `no route to host` 且節點連線迴圈就此停止；再重啟一次後節點恢復 `Ready`，並收到 Pod 指派事件。
- `maccluster-agent netcheck` 從運作中的 agent 回報：直連控制平面成功、沒有設定代理、節點憑證將於 2026-10-27 11:45 到期（警告）。
- **未能解釋：** 這台上的 Python socket 同一時間仍回傳錯誤碼 65，而新的 agent 卻連得上。可能是 MacCluster 在這段時間被授予了區域網路權限、或系統對新簽署二進位的處理方式不同，**我沒有證據分辨，也沒有看到系統設定當時的狀態**。同時說明：agent 的節點連線迴圈出錯一次就停止、不重試，是仍未修的缺口。
- 沒有驗證 App 的「健康檢查」分頁畫面（使用者未授權我操作 App 視窗）；只驗證了其後端 `netcheck` 的輸出。

### 2026-10-22：重新啟動的 state-dir 參數相容修正

- 截圖的 `flag provided but not defined: -state-dir` 是 service 子命令參數解析失敗。UI 已對服務生命週期命令停止附加此參數；CLI 現在也接受舊版 UI 附加的參數。只有 install/config 會使用傳入目錄；start/stop/restart/status 仍針對原本登記的 job/plist，不會因相容參數切換狀態目錄。
- 擴充 TestServiceLifecycle，同時驗證無參數及舊版 state-dir 參數的 start（已載入／未載入）、stop、restart、status，並檢查 launchctl job/plist 仍正確。Go service/supervisor 相關測試通過。
- 完整 build.sh --no-repair 成功，清除 Swift/UI 快取並保留 Go 快取。發現重開 dist App 時，macOS 沿用舊 AppTranslocation 副本；以 SHA-256 比對確認該副本的 agent 與新成品不同。未修改 quarantine 或系統安全設定，另複製同一新成品至 `.local/restart-fixed/MacCluster.app` 開啟，取得新 AppTranslocation 路徑。
- Computer Use 實際按新版「重新啟動 → 重新啟動」，管理頁回到「管理程式運行中」，沒有參數錯誤。再比對 LaunchAgent 登記的 agent 與 dist 新成品 SHA-256 相同；實際 `service status --state-dir /ignored/legacy/path` 成功，證明舊參數不會切換目標。SuccessfulExit=false 的 SIGKILL 政策保持不變。
- 重啟前受管 clusters、workloads、localWorkloads 均為空；未操作外部 Docker 容器，也不據此宣稱已驗證帶工作接管。建置及觀察證據存於未納管的 `.local/startup-ui-fix/restart-compat-build.log`、`restart-result.json`、`restart-stability.json`。

### 2026-10-22：隱藏 Agent 未連線細節與同名程序調查

- 管理程式頁未運行時移除狀態列下方的原始錯誤段落，保留「未運行」。一般啟動、操作及環境訊息顯示前過濾 `agent unavailable`；健康檢查的 agent 摘要改顯示「未連線」。內部錯誤狀態與 CLI 診斷仍保留，不把連不上偽裝成運行中；其餘啟動／操作錯誤仍會呈現。
- UI refreshSnapshot 每輪同時透過同一個 agent 可執行檔呼叫 status、inventory、env list --json，結束後間隔 3 秒再更新。CLI 查詢不是 serve，不會啟動常駐服務；inventory 直接查 Docker，與 agent 是否運作無關，不能只因 Agent 停止就停掉 Docker 清單更新。
- 程序取樣：常駐 serve PID 47116 的父程序為 shell supervisor PID 47114；另有父程序為 GUI PID 45722 的短暫 inventory 程序，例如 PID 47995、48071，約每 3.3 秒出現。這可解釋同名程序重疊；截圖已消失的 PID 47160 未取得完整命令，不能斷言它的確切子命令。
- 實際 UI「停止 → 只停止管理程式」前確認 clusters/workloads/localWorkloads 均為空。停止後另取樣 35 秒，僅捕捉到 GUI 發出的 inventory 查詢（如 PID 48990、49058），未出現 serve；launchctl print 退出 113，確認 job 已卸載。取樣過短可能漏掉 status/env 等更快的命令，不宣稱所有子命令都被捕捉。
- Swift AgentRecoveryTests、JoinProgressTests、HealthCheckTests 共 23 項通過。第一次測試遇到既有 Swift 模組快取衝突，清除 app/.build 後通過。證據位於未納管 `.local/agent-process-check/`。
- 完整清快取建置及簽章驗證成功。以同一成品的獨立副本避開舊 AppTranslocation 後，實際 UI 停止管理程式：管理頁只顯示「未運行」，後續輪詢失敗也沒有原始錯誤段落；同時 CLI status 仍回報 agent unavailable、launchctl print 仍為找不到服務。驗證完成後按「啟動／修復自動啟動」恢復運行，並比對已登記 agent 與 dist 新成品雜湊一致。沒有停止或操作外部 Docker 容器。

### 2026-10-22：Agent 斷線提示、停止說明換行與 GUI 不再啟動 Agent 查詢

- 主畫面在 Agent 斷線時顯示簡短橘色提示；有舊狀態時標示顯示上次資料，沒有狀態時提示到設定啟動。管理程式頁仍僅顯示未運行，不呈現原始 socket 路徑或 agent unavailable。
- 停止確認訊息將兩種選項的標題、說明及解除安裝提醒分行分段。
- 新增 AgentSocketClient，以 App 內的 Darwin Unix socket HTTP 實作 GET/POST、Content-Length/chunked、總 deadline、HTTP 錯誤。AgentClient 將 status/netcheck/logs 與 Agent 操作直接送到 socket；不存在或拒絕連線時回報未連線，不啟動 CLI、工具或 Agent，也沒有 CLI fallback。
- Docker、本機環境及 launchd 操作使用另外建置且簽署的 maccluster-tools。本機工具有明確命令白名單，拒絕 serve、status、netcheck、run-node 等；service config/install 明確登記同目錄的 maccluster-agent，而非工具自身。既有 agent 檔案鎖保護同一狀態目錄只允許一個 serve，原有 CLI 相容性保留。
- Go service/supervisor、工具白名單及第二次 serve 不得替換既有 socket 的測試通過；實際編譯 tools 模式後，驗證 serve/status/netcheck/run-node 拒絕、空環境列出成功、plist 指向 agent。build.sh 測試通過，保留 Go 快取／清除 UI 快取行為不變。
- Swift socket、AgentRecovery、JoinProgress、HealthCheck 整合 32 項通過；包括缺失 socket 時即使提供可執行工具也不呼叫、query 參數轉義、GET/POST、chunked、連線拒絕及逾時。獨立 scratch 避免既有 Swift PCM 衝突。日誌於未納管 `.local/single-agent/`。
- 完整 build.sh --no-repair 建置與簽章成功，實際開啟新版成品。停止前經 socket 確認 clusters/workloads/localWorkloads 均為空；未操作外部容器。Computer Use 確認停止視窗的兩個標題各自換行、說明分段；停止後主畫面顯示「無法與管理程式連線，顯示上次取得的資料」，未呈現 raw socket 錯誤。
- 停止後 20.03 秒、236 次程序快照，maccluster-agent 數量最大 0；包含主畫面手動重新整理。launchctl print 確認找不到服務。只有獨立本機工具短暫更新 Docker/環境清單，沒有啟動 Agent 取狀態。
- 明確按啟動後恢復 Ready，主畫面斷線提示消失；另觀察 20.03 秒、210 次快照，只有同一個 maccluster-agent PID 73766，數量最大 1，包含手動重新整理。兩個成品資源均與實際登記路徑 SHA-256 相同，SuccessfulExit=false 政策保留。取樣結果在 `.local/single-agent/stopped.json`、`running.json`。程序取樣是實測區間證據，App 不 spawn Agent 查詢另由傳輸路由與 regression test 保證；不宣稱能阻止使用者或其他 App 自行執行相容 CLI。

### 2026-10-22：加入預設名稱可見、取消產品前綴限制

- 原「預設」按鈕清空欄位，容易讓使用者以為沒有作用。現改為立即填入整理後的主機名稱加隨機識別碼；處理大小寫、非 DNS 字元、空值及過長名稱。使用者仍可自行輸入或留空交由 Agent 產生名稱。
- 依使用者明確要求，移除 UI、加入 API/bootstrap 與 run-node 的 maccluster- 前綴限制；只保留 Kubernetes 必要的 DNS 名稱驗證。後端留空時的自動名稱前綴只是預設值，不是自訂值限制。歷史驗證紀錄所述前綴規則已被此變更取代。
- Swift JoinProgressTests 6 項通過；Go 自訂名稱驗證及加入錯誤回歸通過，包含不帶產品前綴的 my-macbook-pro、worker01 與 DNS subdomain。正式清 UI 快取建置、簽章成功。
- 舊加入視窗含尚未送出的草稿，首次關閉遭自動審核拒絕。使用者明確確認可關閉並自行重新貼上後才切換新版，未保留憑證到檔案或送出加入請求。實際新加入視窗顯示無固定前綴限制，「預設」動作後 Node 欄位出現非空名稱；App 及已登記 Agent 的新成品已確認。沒有對 control plane 送出加入操作，外部容器保持原狀。日誌在未納管 `.local/join-default/`。

### 2026-10-22：移除 Node 版本的 prototype 後綴

- NodeStatus.nodeInfo.kubeletVersion 由 v1.28.0-maccluster-prototype 改為 v1.28.0-maccluster，同步節點比較文件。
- 本次唯讀確認本機 Agent 的 clusters/localWorkloads 均為空，未直接修改 control plane Node。遠端既有版本需由新版 Agent 連線回報更新，不宣稱已驗證 kubectl 的實際輸出。建置紀錄位於未納管 `.local/node-version/build.log`。
- 完整清 UI 快取建置與簽章成功。套用前再次確認沒有受管叢集或工作後，透過隨附本機工具 stop/install/start 更新本機服務，Unix API 恢復 Ready；沒有額外執行 agent CLI 查狀態。現行登入服務指向 dist 新成品，遠端 Node 欄位仍需實際加入後回報驗證。
