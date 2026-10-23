# Mac Service 網路與 kubelet 試作

已在獨立測試節點驗證 Linux Pod → Service → Mac Docker／原生程序。這是 opt-in TCP Service 轉送，不是 CNI，也不提供 Pod IP、Mac 端 kube-proxy 或完整 Kubernetes 相容性。既有背景管理程式尚未切換到此版本。

## 資料路徑

Linux Pod → 叢集 DNS / ClusterIP → Linux 節點的 Service 資料平面 → EndpointSlice 的 Mac InternalIP:動態埠 → Mac 轉送器 → loopback Docker published port / Native 程序。

每個副本取得不同入口埠。Pod Ready=false、刪除或 node lease 到期時，由 **Linux 叢集內** controller 移除其 EndpointSlice，避免 Mac 斷線時無人清理。仍有 reconcile 與 lease 過期延遲；已建立的 TCP 連線不會搬到新副本。Controller 若自己停止或無法寫入 API，不能保證清理，應監控並由 Deployment 重啟。尚無 leader election，部署只設一份。

## 啟用方式

每個 cluster profile／獨立 worker 的狀態目錄放 `node-options.json`（另有 `nativeBackend`、`launchdUID`，見 [launchd-executor-design.md](launchd-executor-design.md)），目前尚無設定表單：

```json
{
  "advertiseIP": "192.0.2.10",
  "serviceNetwork": true,
  "reservedCPUMilli": 1000,
  "reservedMemoryBytes": 12884901888
}
```

IP 必須屬於本機介面，且 Linux 節點可達。只綁定指定 IP，不修改主機路由、防火牆或虛擬網卡。入口目前沒有網路隔離或應用層驗證，LAN 上能到該 IP:埠的主機也能連線；應由服務本身或現有網路邊界控管。

Node Capacity 取主機真實 CPU／記憶體，Allocatable 扣保留量，再依同一管理程式的活動叢集數分配。這是 scheduler 預算，**不是 macOS 程序硬限制**；其他 App、Docker VM 與獨立 worker 不會自動共享 admission 計數。Metal 記憶體資源尚未實作，沒有假報 GPU 容量。MemoryPressure 取 macOS 訊號，DiskPressure 取 agent state volume，PIDPressure 回 Unknown。無 Pod overlay 所以 NetworkUnavailable 保持 True。

Pod template 需 `maccluster.example.com/managed: "true"` 與 `maccluster.example.com/network: service`，Native 另加 executor label。Service 使用相同 network label，**不設 spec.selector**，以 annotation `maccluster.example.com/service-selector` 放 JSON label map。範例見 `examples/network/smoke.yaml`。只支援一個 container、TCP 與已宣告埠；預設動態埠模式仍受現有單一工作埠限制，多埠須符合固定埠模式限制。

Controller 只處理 `--namespaces` 明確列出的 namespace；自己的 EndpointSlice 有 managed-by label 與 Service UID ownerReference。一般 selector Service、外部容器與其他 controller 的 EndpointSlice 不納管。

## 誰可以連入口埠：`serviceNetworkAllowFrom`（2026-10-20）

入口埠綁在 Mac 的區網 IP 上，**沒有認證**，區網上連得到的主機都能用。節點選項 `serviceNetworkAllowFrom` 限制可以連入的來源（CIDR 或單一位址）：

```json
{ "serviceNetwork": true, "serviceNetworkAllowFrom": ["198.51.100.0/24"] }
```

- **來源位址是叢集節點的 IP**：實測 Pod 的流量經 Service 轉到 Mac 時，Mac 看到的來源是**該 Pod 所在節點的 IP**（Pod 位址在離開節點時被偽裝成節點位址），所以白名單寫節點所在的網段或各節點的 IP 即可。從 Mac 自己的區網位址連入口埠，來源是 Mac 自己的 IP，要通過就得把 Mac 的位址也放進去（用於本機檢查）。
- **預設（沒設）是不限制**，與設定前的行為相同；此時 agent 在節點狀態訊息裡提醒「入口埠對所有連得到這台 Mac 的主機開放」。
- 不在清單內的連線在接受後立即關閉（對方看到連線被重設），不會轉送任何資料；拒絕次數有計數，每個來源每分鐘最多記一行日誌（`service forwarding refused a connection from <IP>`）。修改清單需要重啟 agent（重啟不停工作），對所有入口埠一起生效；已建立的連線不會被切斷。
- 這是來源位址過濾，不是認證：同一網段內被允許的主機仍可使用入口埠；位址可被同網段的主機偽造的環境，需要再搭配網路層的隔離。

## 入口埠怎麼公布（2026-10-20 起）

agent 把 Pod 的入口埠對應寫在 **Pod 狀態的一個 condition**（`type: maccluster.example.com/service-ports`，`status: True`，`message` 是 `{"8080":49463,"http":49463}` 這樣的 JSON；Pod 未就緒時是 `False` 且沒有訊息），controller 從這裡讀。原本是寫在 Pod 的 annotation，但 **Node 授權器不允許節點修改 Pod 的 annotation**（只允許改狀態），所以舊作法在正式的 Node 身分下會被拒絕（測試 worker 以管理憑證運行時沒有發現）。改版後 agent 不再為公布埠做任何額外的 API 呼叫；controller 仍會讀舊的 annotation 當作沒有 condition 時的後備（一個版本）。以真實 Node 身分（`system:node:<測試節點>`）驗證通過，見 validation.md。

## Linux controller

權限範例 `examples/network/controller-rbac.yaml`：讀取 Nodes／Leases，以及指定 namespace 的 Pods／Services；僅對該 namespace EndpointSlices 寫入。Deployment 範例 `controller-deployment.yaml` 尚需替換映像。

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o maccluster-agent-linux ./cmd/maccluster-agent
docker build -f deploy/network/Dockerfile -t YOUR_REGISTRY/maccluster-network-controller:VERSION .
```

Dockerfile 的映像建置／推送尚未實測。現場試驗是獨立 Linux Pod 加上複製的 static binary，該方式不能跨 Pod 重建保存 binary；不可當正式部署。

## kubectl logs 與 port-forward

在上述設定另外提供 `kubeletCert`、`kubeletKey`、`kubeletClientCA` 的絕對路徑，以及 `kubeletPort`（預設 10250；測試為 10450）。Serving certificate 必須含 advertiseIP SAN，API server 也須能信任它。私鑰與 kubeconfig 保持本機 0600，不放版本控制。

Listener 強制 mTLS，只接受指定 CA 簽發的 client certificate；每個請求以憑證 CN/O 發 SubjectAccessReview，要求該 Node 的 nodes/proxy 權限。查不到授權時拒絕。只處理屬於本 Node、runtime UID 相符且未刪除的單一容器 Pod。Port-forward 僅允許 Pod 宣告的 TCP 埠，不能任意代理主機 SSH 等服務。

- `kubectl logs` 對 Docker 與 Native 都支援 `--follow`、`--tail`、`--limit-bytes`、`--timestamps`、`--since`、`--since-time`、`--previous`（階段 4 起；Native 的時間戳精度約 1 秒，見 [docs/kubectl-and-kubelet.md](kubectl-and-kubelet.md)）。
- Native follow 隨當次程序結束而終止，不跨重啟；慢讀者有上限，超出會斷線。`kubectl logs` 只顯示目前這一次執行，之前的執行用 `--previous`。
- Port-forward 走 API server → kubelet 的 SPDY 流程，已對兩種 executor 實測。
- `kubectl exec`、`cp`、`attach` 與節點端點（`healthz`、`pods`、`stats/summary`、`metrics/resource`）已實作；`metrics`、`metrics/cadvisor`、`configz`、TokenReview bearer 認證仍不支援。

測試 listener 目前使用既有管理憑證呼叫 API，**尚未驗證以正常 Node identity 完成這些功能**。待核准的變更只有：簽發有效 24 小時、CN 為 `system:node:maccluster-nettest-20261018` 的 client certificate（signer `kubernetes.io/kube-apiserver-client-kubelet`、O=`system:nodes`），以及 `examples/network/node-auth-review-rbac.yaml` 內單一測試節點的 SubjectAccessReview create 權限。這不修改既有節點授權；測試完成後仍須明確清理 RBAC，憑證到期不會自動刪除 RBAC。

## 後續階段

尚待 Node 身分整合驗證、正式 controller 部署、kubelet 服務憑證的取得與續期，以及 UI 設定；之後才是 Metal admission，以及 Mac 原生程序向 ClusterIP／叢集 DNS 的反向連線。現有 DNS 成功的證據只涵蓋 **Linux Pod 查 Service DNS 後連至 Mac**，不代表 Mac 本機能解析叢集 DNS。
