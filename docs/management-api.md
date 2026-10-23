# Management API v2 contract

## Local transport

The CLI connects to `<state-dir>/agent.sock` (0600) in a private state directory (0700). The default is `~/Library/Application Support/MacCluster`. The GUI invokes its bundled CLI client; it does not send HTTP directly. `serve` runs the agent independently of the GUI. CLI failures return a nonzero exit code and an error on stderr. A successful join submission (`accepted:true`) is not yet a successful cluster connection; poll `status` for progress.

## Requests and responses

The Unix socket and CLI stdin transport remain. CLI subcommands `create`, `delete`, `leave`, `configure` POST JSON stdin. `logs --uid UID --cluster-id ID`; empty cluster-id selects local services. `status` retains legacy top-level fields for compatibility and adds:

```
clusters: [{id, name, endpoint, nodeName, phase, message, managementConfigured:bool, workloads:[Workload]}]
localWorkloads: [Workload]
```

Workload retains uid/name/namespace/executor/phase/message/pid/port/restarts plus lastExitAt:string, lastExitCode:int, lastExitReason:string, failureCount:int. Failure history remains visible after automatic restart. Clusters include only this Mac's assigned Pods, not all cluster workloads. `Unknown` means runtime unavailable, not confirmed exited. Disconnected agent means stale data, never display as current healthy.

- join JSON: existing command,nodeName,proxyURL plus `name` optional and `managementKubeconfig` optional absolute path. Each join creates independent cluster profile, credentials and runtime. Reject duplicate API endpoint. Returns `{accepted:true,id:...}`. No token persistence.
- create JSON: `{name,executor:"native|docker",image,command:[...],args:[...],env:{},workingDir,containerPort:0,restartPolicy:"Never|Always|OnFailure"}`. Local service only, backend generates UID; default restart policy Never, docker default executor. Native absolute executable; user can explicitly set `/bin/zsh` args `["-lc","..."]`. Definitions persisted privately, including env; UI should disclose local saving. Returns created Workload. Failed creation remains visible with error status and deletable.
- delete JSON: `{uid,clusterID:"",mode:"pod|workload"}`. Empty clusterID: permanently remove local definition and owned runtime. For cluster: pod deletes one assigned Pod (controller may replace); workload deletes its owning Deployment/StatefulSet/DaemonSet/Job or standalone Pod, requires configured management kubeconfig with permissions; backend verifies target assigned UID and ownership chain. UI confirmations distinguish these effects.
- leave JSON `{clusterID}`: cordon owned node, stop its controller and runtime, delete its registered Node and local credentials/profile. If remote cleanup fails, retain profile with error and support retry; do not claim success or force-forget silently. Management kubeconfig may be required for Node deletion. Local services and other clusters unaffected.
- configure JSON `{clusterID,managementKubeconfig:"/absolute/path"}`: validate endpoint/CA matches and management connection before saving path; does not copy admin credentials. Needed for deletion of workload controllers and remote node cleanup when node credential insufficient.

CLI errors returned stderr/nonzero. API operations show progress and errors rather than optimistically removing rows. Local service create/delete supported offline. No whole-cluster delete operation. Definitions and profiles survive daemon restart. Existing `.local/agent/node.kubeconfig` migrated as profile `legacy` using same runtime directory (avoid orphan ownership mismatch).

`inventory` is a read-only CLI command against the current Docker context, called by the UI independently of the daemon. It lists all container states, excludes `maccluster.owner` and Kubernetes Pod/container labels, and returns Workload fields plus `external:true`, `image`, `ports`. External containers are displayed under local services but are not persisted as desired state, automatically restarted, deleted, or adopted. Inventory errors retain the previous snapshot with a stale-data warning. External logs remain in Docker Desktop. `docker-control` accepts stdin `{containerID,action:"start|stop"}`; it rechecks full ID and ownership labels before manually starting/stopping external containers. Managed/Kubernetes containers are rejected. The UI also offers opening Docker Desktop.

### 遠端刪除 Node

Cluster `phase: Removed` 表示 Node 查詢明確回報 NotFound 或 UID 與已保存的身分不同。`message` 包含偵測時間、原因與可能的本機清理錯誤。此狀態保存於 profile，重新啟動不會重新註冊；運行日誌會顯示紀錄。約每 3 秒驗證一次，API 延遲會增加偵測時間；網路／授權錯誤不視為移除。Removed profile 可透過原 leave 動作清除本機設定，不刪除遠端同名替代 Node；重新加入須清除該 profile 後重新操作加入。

## Execution environments

Host-local registry under `<state-dir>/environments/<name>/env.json` (0600). It is edited directly by the CLI, not through the socket; the agent re-reads it on every launch and on each node-label sync.

- `env add --name N --root DIR [--type link|dir] [--bin-dir DIR] [--workdir DIR] [--extra-path DIR]... [--set-env K=V]...` registers or overwrites. It validates absolute existing directories, rejects reserved variables (`PATH`, `VIRTUAL_ENV`, `MACCLUSTER_PORT`) and names that are not label-safe (lowercase, at most 63 bytes). The linked directory is never modified.
- `env list [--json]` returns every entry: `{name,type,root,binDir,workDir?,extraPath?,env?,available,problem?,executables?}`. Broken entries are included with `available:false` and `problem`; `executables` lists up to 60 executable names from `binDir`.
- `env shell --name N` prints `{executable,args,env[],cwd}` for an interactive shell inside the environment (zsh uses a generated `ZDOTDIR/.zshrc`, bash a generated rcfile, both under `<state-dir>/shell/<name>/` with 0600). The caller owns the PTY.
- `env remove --name N` removes only the registry entry.
- `create` accepts `environment` for native local services; a bare command name is then allowed and resolved inside the environment's bin directory. Unknown environment, `docker` executor, or a command containing `/` that is not absolute is rejected with 400.
- Workloads (`status`) carry `environment` when set. Pods select an environment with label `maccluster.example.com/environment` (requires `executor: native`). Nodes advertise usable environments as `env.maccluster.example.com/<name>=true`.

## Fixed ports

- `create` accepts `fixedPorts` (array of 1–8 distinct ports, native executor only). The agent allocates no port, exposes them as `MACCLUSTER_PORT` (first) and `MACCLUSTER_PORTS` (comma separated), and expands `$(MACCLUSTER_PORT)` / `$(MACCLUSTER_PORTS)` in `args` and `env`.
- Workloads in `status` carry `port` (first or allocated) and, in fixed-port mode, `fixedPorts` (the JSON field is not `ports`, which already means the published ports string of an external Docker container).
- A port that is busy (another workload of the agent, or any listener on the host) keeps the workload `Pending` with `fixed port N is already in use[ by NAME]`; it is retried every reconcile and is not counted in `restarts` or `failureCount`.
- Pods select fixed ports with label `maccluster.example.com/fixed-ports` (`"8100"` or `"8100_9100"`); see docs/fixed-ports.md for the validation rules.

## Docker container ports in the inventory

`inventory` fills `ports` from `docker ps`, which lists only ports that are published right now. For containers that are not running and show no ports there, the mappings they were created with are read from their configuration (`HostConfig.PortBindings`, then `Config.ExposedPorts`) in one `docker inspect` call and rendered like `docker ps` (`0.0.0.0:3000->8080/tcp`); `portsConfigured:true` marks these so the UI can say they are settings, not live mappings. Running containers keep Docker's own value. A failed lookup leaves `ports` empty and never fails the inventory.

## Kubernetes lifecycle event journal

`status.events` contains the newest retained 1,000 events in insertion order (oldest first). Each event has string fields `id`, `time` (UTC RFC3339Nano), `clusterID`, `clusterName`, `namespace`, `workload`, `kind`, `level`, `message`. `level` is `info`, `warning`, or `error`. The UI uses the existing ANSI green/yellow/red palette with visible level labels; JSON stores structured severity, not escape codes.

The agent records join requests/results, leave requests/results, externally removed/replaced Nodes, assigned Pods, observed Pod phases, and removed assignments. Collection runs in background reconciliation, not status polling. Successful Pod list results drive observations; a failed API query never means all Pods disappeared. `events.json` is private (0600), bounded, independent of cluster profiles, and keeps deduplication state across agent restarts. Leaving a cluster keeps its event history. Messages are fixed summaries and do not contain join tokens, commands, env values, or raw errors.

This is a host-local event journal, not Kubernetes audit logging: recording starts with the updated agent, has no historical backfill, and can miss transient changes between reconciliation rounds. Persistence failures are retried; events not yet saved can be lost on process failure.
