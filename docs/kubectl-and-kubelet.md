# kubectl exec／cp／attach、日誌選項與 kubelet 端點

已在測試 worker 以回歸套件 `scripts/node-feature-suite.py` 與 Linux 節點對照驗證；**現行 agent 尚未部署，且現行節點沒有啟用 kubelet 端點**（見最後一節）。

這些功能都經由 kubelet 端點提供：`kubectl` → API server → 這台 Mac 上 agent 的 kubelet 伺服器（mTLS，每個請求用 SubjectAccessReview 檢查 `nodes/proxy`、`nodes/stats` 或 `nodes/metrics` 權限）。所以它們只在該節點啟用了 kubelet 端點時才可用（`node-options.json` 的 `kubeletCert`／`kubeletKey`／`kubeletClientCA`／`kubeletPort`，見 [docs/service-network.md](service-network.md)）。

## kubectl exec、cp、attach

| 指令 | Docker | Native |
| --- | --- | --- |
| `kubectl exec POD -- CMD` | `docker exec` 進入容器 | 在該程式的**環境變數與工作目錄**下**另起一個程序**執行；不是進入原程序，也不在它的命名空間或私有檔案系統 |
| `-i`（stdin） | 支援 | 支援 |
| `-t`（終端機） | 支援，含終端機大小調整 | 支援，含終端機大小調整（自己配置 pty） |
| 結束碼 | 回報為 `command terminated with exit code N` | 同左（被訊號終止時為 128＋訊號） |
| `kubectl cp` | 透過 exec 與 `tar`：容器內需要有 `tar` | 透過 exec 與主機的 `tar`；路徑是 Mac 的路徑 |
| `kubectl attach` | `docker attach` 到主程序（無 stdin 時加 `--no-stdin`） | 程式的輸出是檔案而不是串流，所以 attach 是「從現在起跟隨輸出」；沒有輸入可接，要求 stdin 時會在 stderr 說明 |

限制：同一個節點同時最多 128 個串流（exec、attach、logs、port-forward 共用）；容器必須在執行中；Pod 須綁定在這個節點且 UID 與引擎相符，否則回 404。

## kubectl logs 選項

| 選項 | Docker | Native |
| --- | --- | --- |
| `--follow`、`--tail`、`--limit-bytes` | 支援 | 支援 |
| `--timestamps` | `docker logs --timestamps` | 支援，見下 |
| `--since`、`--since-time` | 支援 | 支援，見下 |
| `--previous` | 支援 | 支援 |

**只顯示目前這一次執行。** 與 kubelet 相同，`kubectl logs` 只顯示目前的容器；`--previous` 顯示上一次執行。Native 每次重新啟動時，上一次的日誌與時間索引會被移到 `*.prev.log`（只保留一代，再前一次被取代）；Docker 同一個容器的所有執行共用一份日誌，agent 以啟動時間當邊界（`docker logs --since/--until`）。App 的日誌頁仍顯示所有執行的歷史。沒有上一次執行時，`kubectl logs --previous` 回 `previous terminated container "c" in pod "p" not found`。

**Native 的時間戳怎麼來的：** 程式直接寫檔，沒有人記錄每行何時寫入。agent 每 200 毫秒檢查一次檔案，在 `<日誌>.ts` 記下每批新資料第一次被看到的時間（同一秒內合併為一筆，索引才不會比日誌還大）。一行的時間是包含它第一個位元組的那一批的時間，**精度約 1 秒**。agent 不在期間寫入的輸出，會被標成 agent 接管後第一次看到的時間；在此版本之前寫入、沒有索引的輸出，用檔案的修改時間。`--follow --timestamps` 的即時行以到達時間標記，跨越封包的一行只加一次前綴。

## 節點端點（經 API server 代理）

| 端點 | Linux 節點 | Mac 節點（測試 worker） |
| --- | --- | --- |
| `healthz` | 有 | 有 |
| `pods` | 有 | 有（綁定到此節點的 Pod） |
| `stats/summary` | 有 | 有：節點與每個 Pod／容器的 CPU（累計與當下 nanocores）、記憶體工作集、磁碟 |
| `metrics/resource` | 有 | 有：metrics-server 讀取的 `node_*`、`container_*`、`pod_*` 家族與 `resource_scrape_error` |
| `metrics`、`metrics/cadvisor`、`configz`、`spec/` | 有（`spec/` 除外） | 沒有（回 404） |

授權：`healthz`、`pods` 要求 `nodes/proxy`，`stats/*` 要求 `nodes/stats`，`metrics/*` 要求 `nodes/metrics`（與 kubelet 的規則相同），皆只允許 GET。

**資源用量從哪裡來：**
- Docker 容器：Docker Engine API 的 `/containers/<id>/stats`（累計 CPU 奈秒與記憶體；工作集＝使用量減 inactive file，與 kubelet 相同）。
- Native：`ps` 對該程序**群組**加總 CPU 時間與 RSS。
- 節點：CPU 是把全機的 `ps %cpu` 負載對時間積分成累計值（macOS 沒有這種計數器，metrics-server 取兩次取樣的差算出速率）；記憶體工作集是活躍、固定與壓縮的分頁。這是估計值，不是精確計量。

**`kubectl top`：** 需要叢集裡有 metrics-server，這個叢集沒有部署，所以端點是直接驗證的（回應內容與格式），`kubectl top` 本身沒有實測。Linux 節點同樣在沒有 metrics-server 時回 `Metrics API not available`。

## 已實測

回歸套件（`scripts/node-feature-suite.py`）對 Linux、Mac Docker、Mac 原生三個目標：`exec`（輸出、結束碼 7、stdin）、`cp` 進出、`attach` 看到輸出、`--timestamps` 每行皆有 RFC3339 時間、`--since` 的取捨、`--previous` 在容器重啟後顯示上一次的輸出、節點端點的內容與對應的 Pod 資料，全部一致。單元測試另外涵蓋 TTY 與大小調整、tar 串流、授權的子資源、時間索引與跨封包行等。

## 啟用 kubelet 端點：現行節點尚未做

現行 agent 的節點沒有 kubelet 端點（埠回報 0，API server 的代理被拒），所以 `kubectl logs`／`exec`／`cp`／`attach`／`port-forward` 目前對它都不可用。要啟用需要一張 kubelet **服務憑證**，包含 Mac 的 IP 為 SAN，且 API server 要能接受它。

| 取得方式 | 取捨 |
| --- | --- |
| 由 agent 自己建立 `kubernetes.io/kubelet-serving` 的 CSR（金鑰留在本機），等叢集核准後安裝與續期 | 標準做法，與客戶端憑證續期對稱。**`kubelet-serving` 的 CSR 預設不會被自動核准**，需要管理員執行 `kubectl certificate approve`，或叢集裝 `kubelet-csr-approver` 之類的核准器 |
| agent 自簽憑證 | 不需要管理員，但只有在 API server 不驗證 kubelet 憑證（沒有 `--kubelet-certificate-authority`，kubeadm 預設如此）時才可用，安全性較弱；這個假設還沒有實測 |
