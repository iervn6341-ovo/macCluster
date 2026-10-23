# 固定埠（多埠）

> **狀態：已實作並在真實叢集驗證。** 需要新版背景 agent：**舊版 agent 會忽略
> `maccluster.example.com/fixed-ports`**，Pod 仍使用分配埠。已驗證與未驗證範圍見文末「驗證」。

## 為什麼需要

原生工作預設由 agent 為每個副本挑一個隨機本機埠（`$(MACCLUSTER_PORT)`），就緒探測也一律打這個埠。
程式如果寫死自己的埠（例如 vLLM 的 `--port 8100`，且要讓叢集其他節點用 LAN IP 連線），探測就會打到沒人在聽的
隨機埠，Pod 永遠 `READY 0/1`，即使服務正常（實例：`agent-kv-vllm-metal`，見 docs/validation.md）。

## 行為

在 **Pod template** 加一個 label，宣告程式會自己綁定的埠：

```yaml
labels:
  maccluster.example.com/fixed-ports: "8100"        # 單一埠
  maccluster.example.com/fixed-ports: "8100_9100"   # 多個埠，以底線分隔
```

label 值不能含逗號，所以多個埠用 `_` 分隔。不含此 label 的工作行為完全不變。

### agent 的行為（固定埠模式）

1. 不分配隨機埠。每個宣告的埠都原樣使用。
2. `MACCLUSTER_PORT` 設為**第一個**埠；另設 `MACCLUSTER_PORTS`（逗號分隔的全部埠）。`$(MACCLUSTER_PORT)`、`$(MACCLUSTER_PORTS)` 在 `args`／`env` 中展開。
3. 就緒探測打探測自己宣告的埠（`readinessProbe.httpGet.port`），不再改用分配埠。
4. 狀態回報（`status` 的 workload）除 `port`（第一個埠）外，新增 `fixedPorts`（全部）。（不叫 `ports`：外部 Docker 容器的 `ports` 已是字串。）

### 驗證規則（不符合就維持 Pending 並顯示明確訊息）

| 規則 | 訊息方向 |
| --- | --- |
| 值為 1–65535 的整數，以 `_` 分隔，不可重複，最多 8 個 | invalid fixed-ports value |
| 只支援 native executor | fixed-ports requires executor=native |
| 每個 `containerPort` 都必須在清單內（固定埠模式放寬「只能一個埠」的限制） | containerPort X is not in fixed-ports |
| `readinessProbe.httpGet.port` 必須在清單內（探測仍只有一個） | readiness port is not in fixed-ports |
| 啟動前每個埠都必須未被占用（含同一 agent 的其他副本） | fixed port N is already in use |

### 副本與更新

- 固定埠不能被兩個程序同時使用，所以同一 Mac 上只能有一個副本。第二個副本維持 Pending 並說明埠被占用，不會悄悄失敗。
- 更新請用 `strategy: Recreate`：舊程序完全退出後才啟動新的。
- 重啟時埠不變，外部用戶端的位址可以固定。

### 注意

- 程式若綁 `0.0.0.0`，埠會對整個區網開放。沒有 NetworkPolicy，vLLM 預設也沒有認證。
- 啟動前的「埠是否空閒」檢查與程式實際綁定之間仍有競爭窗口（與動態埠相同）。
- 這項功能不提供 Pod IP、Service 或 DNS；其他節點只能用 Mac 的 LAN IP 加埠連線。

## 為什麼用 label

- 與現有設計一致（`executor`、`environment` 都是 label）。
- 可用 `kubectl get pods -l maccluster.example.com/fixed-ports=8100` 篩選。
- 值只有數字與分隔符，沒有字元限制問題。

## 範例

`docs/planned/fixed-ports.yaml`（App 的 FAQ「套用 YAML」分頁顯示同一份，並標示規劃中）。

## 實作位置

- `internal/cluster/runner.go`：`FixedPortsLabel`、`parseFixedPorts`（解析與驗證）、`resolveProbePort`（數字或具名探測埠）、固定埠模式下探測打探測自己宣告的埠。
- `internal/runtime/types.go`：`Spec.FixedPorts`、`Instance.Ports`。
- `internal/runtime/engine.go`、`fixedports.go`：固定埠啟動（不分配埠）、`MACCLUSTER_PORT(S)` 與展開、占用檢查（引擎內其他工作＋整機監聽）、`portBusyError`（維持 Pending，不計入重啟或失敗次數）。
- 本機服務建立（`create`）接受 `fixedPorts`（JSON 陣列）；狀態的 workload 含 `fixedPorts`。

## 驗證

- 單元測試（`go test -race ./...`）：label 解析（非數字、0、過大、前導 0、逗號、重複、空元素、第 9 個埠、containerPort 或探測埠不在清單、未知具名埠、docker executor）、多埠啟動與環境變數／參數展開、動態埠行為不變、被外部程序占用時維持 Pending 且不計數並在釋放後啟動、同引擎第二個工作無法共用埠、探測打宣告的埠而非第一個埠。
- 獨立 agent 端到端（另一個狀態目錄、不含叢集）：實際 Python 程序同時服務兩個固定埠（皆回 200）；第二個工作要求重疊的埠時 Pending（訊息 `fixed port 18102 is already in use by fixed-a`、restarts 與 failureCount 為 0）；第一個刪除後第二個自動啟動並服務。
- 真實叢集：輕量工作兩個固定埠 Ready 且探測打宣告的埠、第二副本 Pending 且不計次數、非法設定給出明確訊息；細節見 docs/validation.md。
- **尚未驗證**：vLLM 實際在固定埠由 agent 判定就緒（需在 `agent-kv-vllm-metal` 的 Deployment 加 label，會重建 Pod）。本機服務建立表單尚無固定埠欄位。
