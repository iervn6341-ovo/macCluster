# Todo

來源：2026-10-16 專案分析；各項依實測更新。

## 網路與連線

- [ ] 查出 Go／kubectl 直連 control plane 回報 no route to host 的根因（curl 與 SSH 可達；疑似 macOS 本機網路權限），取代臨時 SSH SOCKS 隧道。
- [x] Linux Pod → Service DNS／ClusterIP → Mac Docker／Native TCP，已以動態 EndpointSlice、replicas、readiness 與 lease 過期實測；尚非 Pod overlay，見 docs/service-network.md。
- [ ] 驗證 Flannel 與 macOS 的雙向路由／隧道、Pod 位址映射、ClusterIP、DNS。
- [ ] 查明 vLLM 重載後 `/health` 逾時；模型在 NFS `198.51.100.1`，TCP 2049 回報 no route to host，可能與上一項同類。

## 資源與生命週期

- [ ] Metal 記憶體 admission（例如擴充資源 `maccluster.example.com/metal-memory`），並以實測值取代寫死的 2 CPU／8Gi。
- [x] 獨立節點的 `kubectl logs`（含 follow）與 `port-forward` 已對 Docker／Native 實測；正常 Node 身分授權待核准驗證。
- [ ] `kubectl exec`、metrics 與正式 kubelet 憑證續期。
- [ ] 節點憑證自動輪替（目前七天）。
- [ ] Native 在 Agent 崩潰後的接管，以及跨節點重複執行的 fencing。

## 原生執行環境

- [x] 固定埠（多埠）：以 `maccluster.example.com/fixed-ports` 宣告程式自行綁定的埠，探測改打宣告的埠。程式、單元測試與獨立 agent 端到端已完成，見 docs/fixed-ports.md。
- [x] 固定埠在真實叢集以輕量工作驗證完成（2026-10-18）。
- [ ] 在 `agent-kv-vllm-metal` 加 `fixed-ports: "8100"` 驗證 vLLM 實際固定埠就緒（需該 Deployment 擁有者同意，會重建 Pod）。
- [ ] 本機服務建立表單加入固定埠欄位。

- [x] 階段 1：環境登記（CLI）、`environment` label 解析、Node label 公布、Pending 原因。已通過 `go test -race ./...`；尚未在真實叢集用 vLLM 驗證。
- [x] 階段 1 實測（輕量 Python 工作）：Node label 同步、RBAC、環境套用、缺環境 Pending 均通過，見 docs/validation.md。
- [x] 用 `vllm-metal` 環境載入真實 vLLM（gemma-3-4b-it）已通過，見 docs/validation.md；`examples/vllm.yaml`（E4B）已改為環境版但未實際套用。
- [ ] Metal 記憶體 admission：第二份模型能否並存目前只能人工判斷（`memory_pressure` 指標波動大，不適合當精確容量）。
- [ ] 根因已查明：每次 ad-hoc 重簽使 TCC「網路卷宗」授權失效，提示無人回答導致 `open()` 阻塞。待決定：回答提示，並改用穩定簽章身分（自簽憑證或 Developer ID）避免每次重建重問；另檢查 Go 直連 LAN 失敗是否為 Local Network 權限。見 docs/validation.md。
- [x] 階段 2（調整後）：環境管理 UI、內嵌終端機、語言中立；不含建立 venv 與專案檔案（使用者決定）。尚缺實際視窗操作驗證，見 docs/validation.md。
- [x] 重啟背景 agent 讓 `environment` 欄位生效：已完成並驗證；授權提示再次出現並由使用者允許。
- [ ] 在實際視窗驗證 App 側邊欄「執行環境」區、表單、移除確認、NSOpenPanel 與終端鍵盤輸入（Computer Use 找不到此 App，離屏渲染不畫側邊欄）。
- [ ] 階段 3：版本指紋（可選的版本探測指令加 lockfile 雜湊）公布為 label／annotation、環境變更提示。
- [ ] 若要管理專案檔案／腳本／文件：先定義範圍（目前不做）。

- [x] 現行節點的 client 憑證已在 2026-10-20 03:45 GMT 自動續期成功（新憑證有效至 2026-10-27，節點全程 Ready；舊憑證保留為 node.kubeconfig.prev）。續期失敗只寫 stderr、LaunchAgent 無 log 路徑的問題仍待補。
- [ ] Mac 出站 Service／DNS：實測 ClusterIP 與 kube-dns 皆不可達；需設計本機解析與代理方案。
- [ ] Network controller：加入 leader election、Reconcile 錯誤不退出，並建置映像與 Deployment。
- [ ] 強制續期驗證（暫緩）：在 agent 加入 `rotate-now` 觸發檔（state-dir 內出現時忽略 70% 門檻續期一次並刪檔），重建並重啟 agent 後驗證續期與 session 重啟後節點 Ready。需修改 `cmd/maccluster-agent/management.go` 與 `internal/cluster/rotate_test.go`；代價是再重啟一次 agent（本機 vLLM 重載、可能再出現網路卷宗授權提示）。重啟前備份 `.local/agent/node.kubeconfig`。若 2026-10-20 約 03:49 GMT 後已自然觸發並成功，可改為只確認結果而不需要此項。
- [ ] 同一 Mac 以多個 agent 加入同一叢集時，各 agent 各自回報整機容量會超額登記；需跨 agent 的容量分攤或明確的 allocatable 設定。
- [ ] `maccluster-nettest-20261018` Node 目前 NotReady（worker 已停止），確認後依 validation.md 的清單清理。
- [x] 原生程序接管（A）：標記檔含程序身分、啟動時核對並接管、日誌直接寫檔與輪替、Logs() 讀歷史（2026-10-19，已用隔離 agent 驗證，尚未部署到現行 agent）。
- [ ] 正常關閉（SIGTERM）時保留 Docker 容器與（可選）原生程序，重啟後接回；容器日誌在刪除前保存。
- [x] 現行安裝已遷移到 `~/Library/Application Support/MacCluster` 並部署新版 agent（2026-10-19）。待辦：確認穩定後清除舊 `.local/agent` 與 `.local/backup-agent-20261019*`（含憑證）；管理用 kubeconfig 仍在 `.local/admin.kubeconfig`，是否搬入 Application Support 另議。
- [ ] 部署接管身分修正（PID＋啟動時間＋程序群組，不比對命令列）到現行 agent；現行版本在轉接殼啟動的程序被當機後接管時會失敗。與下次 agent 重啟一起做。
- [ ] 網路測試腳本 `check.py`／`lease-check.py` 的計數改為只算 Mac 測試工作負載（目前以 `*-mac-only.py` 副本繞過）。
- [ ] 依 [docs/node-gap-roadmap.md](node-gap-roadmap.md) 逐項解決 Mac 節點缺口（階段 0 快速改善 → 1 環境變數與資源 → 2 探針與生命週期 → 4 exec 與日誌 → 3 卷宗 → 5 網路 → 6 多容器）。先做階段 0；需使用者決定的事項列在該文件末段。
- [x] 階段 0（docs/node-gap-roadmap.md）已在測試 worker 驗證，回歸套件 `scripts/node-feature-suite.py` 全綠。待辦：與下次 agent 重啟一起部署到現行 agent；下一步階段 1（環境變數 valueFrom／envFrom、Docker 資源 limits、securityContext 子集）。
- [x] 階段 1（環境變數來源、資源 limits、securityContext 子集）已在測試 worker 驗證（docs/env-and-security.md）。待辦：與階段 0 一起部署到現行 agent；下一步階段 2（探針與 lifecycle hooks）。
- [x] 階段 2（探針、lifecycle hooks、優雅停止）已在測試 worker 驗證（docs/probes-and-lifecycle.md）。待辦：與階段 0、1 一起部署到現行 agent；下一步階段 4（`kubectl exec`／`cp`／`attach`、日誌選項、kubelet 端點）。
- [x] 階段 4（exec／cp／attach、日誌選項、kubelet 端點 4.1–4.3）已在測試 worker 驗證（docs/kubectl-and-kubelet.md）。待辦：階段 4.4（現行節點啟用 kubelet 端點：服務憑證取得與續期、正常 Node 身分授權驗證）；與階段 0–2 一起部署到現行 agent；下一步階段 3（卷宗）與階段 5（網路）都需要先做設計並由你決定。
- [ ] 階段 3（卷宗）設計完成，決策 2–4 已決定（Secret 用一般檔案、token 只在明確 true 時掛載並注入 KUBERNETES_SERVICE_*、hostPath 預設拒絕加白名單）；決策 1 也已決定：Docker 先做（3a–3c），Native 最後用環境變數與展開補上；四項決策都已完成，等你下令開始 3a，見 docs/volumes-design.md。
- [ ] 階段 5（網路）設計完成，等待決策：出站方案（建議先試 Mac 路由經 Linux 閘道）、閘道節點、入站控制器生產化與映像推送、現行 agent 部署時機、網路診斷，見 docs/network-design.md。
- [ ] 原生工作改由 launchd 管理（Agent 與工作分離）：設計見 docs/launchd-executor-design.md，待先做 launchctl／Metal 實測。
- [ ] launchd 後端：決定何時部署到現行 agent（需重啟、vLLM 重載）；root 身分與登出行為未驗證。見 docs/launchd-executor-design.md。
- [x] 環境終端機輸出保存（含自動清除）：已實作，見 docs/terminal-log-design.md。
- [x] 原生服務自行導向的日誌由 agent 自動接上：已實作並在測試 worker 驗證；待部署到現行 agent（需重啟）。見 docs/log-redirect-design.md。
- [ ] 出站代理啟用到現行節點：映像推送、`maccluster-egress` namespace 部署、`egressProxy` 節點選項；測試 namespace 的 egress-gw 資源待清除。見 docs/egress-design.md。
- [x] 缺口 4：agent 正常重啟不停掉工作：已實作並在測試 worker 驗證；待部署到現行 agent（安裝那次重啟仍會停止工作）。見 docs/agent-restart-design.md。
- [x] 缺口 3：現行節點已啟用 kubelet 端點（自簽憑證，2026-10-19）。回復：刪除 node-options.json 並重啟。見 docs/production-enable-design.md。
- [ ] keep-awake 已實作並在測試 worker 驗證；待部署到現行 agent 與 App。見 docs/keep-awake-design.md。
- [ ] Service 網路與 ServiceAccount token：評估與測試方法見 docs/service-network-and-token-plan.md（含探測：Node 身分不能改 Pod annotation，現行 Service 網路設計需改用 pods/status condition），待你決定。
- [ ] ServiceAccount token（3c、3d）已實作並在測試 worker 以 Node 身分驗證；待部署到現行 agent。Service 網路（B）尚未開始，見 docs/service-network-and-token-plan.md。
- [ ] Service 網路（B）後續：controller 正式化（映像、leader election、RBAC、專屬 namespace）、入口埠來源白名單、啟用到現行節點。公布埠改用 condition 已完成並以 Node 身分驗證。見 docs/service-network-and-token-plan.md。
- [ ] Service 網路（B）剩下：controller 正式化（映像、leader election、RBAC、專屬 namespace）與啟用到現行節點；公布埠 condition 與來源白名單已完成並在測試 worker 以 Node 身分驗證。
- [ ] Service 網路 controller 正式化：設計與決定事項見 docs/service-controller-plan.md（映像推送、多架構、namespace、leader election、RBAC）。
- [ ] controller 正式化剩下：推送映像（待 insecure-registries 決定）、測試 namespace 驗證一輪、正式部署到 maccluster-system、啟用到現行節點。離線部分（leader election、健康端點、manifest 產生器、多架構建置）已完成。

## 原生程式實際監聽位址（已實作，尚未部署到現行 agent）

- Agent 在原生程式啟動後以 `lsof -a -p <pid> -iTCP -sTCP:LISTEN -nP -Fn` 只偵測一次（宣告的埠都出現即停；沒出現則放棄），結果寫入 `*.native` marker，agent 重啟後直接沿用，不再詢問。
- `GET /workloads` 的 `listening: [{port, addresses}]`；App 的「可連線範圍」依此顯示（所有網路介面／僅此 Mac／特定 IP），未偵測到時顯示「尚未開始監聽（偵測中）」。
- 容器資訊頁改為從頂端開始顯示（移除自動捲到日誌），「容器資訊」「連接埠」為可折疊卡片，展開狀態記在 App 偏好。
