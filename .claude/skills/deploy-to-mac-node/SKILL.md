---
name: deploy-to-mac-node
description: How to deploy a service from a Kubernetes control plane onto a macOS node managed by MacCluster (a Virtual Kubelet style node that runs Docker Desktop containers or native macOS processes with Metal). Use this skill whenever the user wants to run, schedule, scale, update, debug or remove a workload on the Mac node, mentions a node named maccluster-*, the maccluster.example.com/executor or /environment labels, Apple Silicon or Metal GPU workloads such as vLLM-Metal or MLX in a cluster, or asks why a Pod assigned to the Mac is Pending, unschedulable or rejected, even if they never say "MacCluster".
---

# Deploy a service onto the MacCluster Mac node

The Mac joins the cluster as a Node, but it is not a normal kubelet node. A local agent
(`maccluster-agent`) watches the Pods assigned to this node and runs each one either as a
**Docker Desktop container** (default) or as a **native macOS process**. Native processes are
the reason this node exists: Docker on macOS runs in a Linux VM and cannot use Metal, while a
native process can.

Because the agent supports a deliberate subset of Kubernetes, most deployment failures come from
a manifest that is valid Kubernetes but outside that subset. Follow the steps below and use the
templates in `assets/` rather than writing a manifest from memory.

## Before you start

1. **Find the node and check it is Ready.**
   ```bash
   kubectl get nodes -l kubernetes.io/os=darwin
   kubectl get node NODE_NAME --show-labels
   ```
   Node labels you will rely on: `maccluster.example.com/host=<node name>`,
   `capability.maccluster.example.com/docker=true`, `capability.maccluster.example.com/native=true`,
   and `env.maccluster.example.com/<name>=true` for each execution environment the Mac advertises.
2. **Pick the executor.**
   - Needs Metal / the macOS GPU, or a macOS-only tool → **native**.
   - Plain Linux container image, no GPU → **docker** (the default; needs Docker Desktop running on the Mac).
3. **Pick a dedicated namespace** for tests (for example `maccluster-test`). Namespaces are not a
   host isolation boundary: a native workload runs arbitrary commands as the Mac's agent user and
   can reach everything that user can. Do not touch other namespaces or the Node object itself
   (labels, taints, deletion) while deploying.
4. **Native only: choose how the program is found.**
   - Absolute path to an executable that already exists on the Mac, or
   - a **registered execution environment** (a named Python venv, node install or any toolchain
     directory). Environments are registered on the Mac by its operator (app toolbar "執行環境", or
     `maccluster-agent env add`), never through kubectl. If the one you need is not in the node
     labels, ask the Mac's operator to register it; do not try to create it yourself.

## The fields every manifest needs

These come from the translator in the agent, which rejects or strands the Pod otherwise:

| Field | Why |
| --- | --- |
| `maccluster.example.com/managed: "true"` in the **Pod template** labels | Prototype safety switch. Without it the Pod is rejected with "explicit … managed=true is required". |
| `nodeSelector: maccluster.example.com/host: <node name>` | Keeps the workload on this Mac. |
| Toleration for `maccluster.example.com/prototype=true:NoSchedule` | The node is tainted; without the toleration the Pod is never scheduled. |
| `automountServiceAccountToken: false` | Leave it `false` unless the program must call the Kubernetes API. Unset also means "not mounted" on this node (plus a Warning event); only an explicit `true` on a **Docker** Pod mounts the token. |
| `maccluster.example.com/executor: native` (native only) | Absent or `docker` means Docker; any other value is an error. |
| `maccluster.example.com/environment: <name>` plus `nodeSelector env.maccluster.example.com/<name>: "true"` (environments only; names are lowercase letters, digits, `-`, `_`, `.`) | Selects the environment and only schedules to a node that advertises it. Requires `executor: native`. |

Put labels under `spec.template.metadata.labels`. Labels on the Deployment's own metadata are not
copied to Pods, which is the most common reason the executor label seems to be ignored.

## Deploy

1. Copy the matching template from `assets/` and replace the placeholders
   (`NODE_NAME`, `NAMESPACE`, `APP_NAME`, the lowercase `env-name`, image or command, port):
   - `assets/docker.yaml` — default executor, container image
   - `assets/native.yaml` — native process from an absolute path
   - `assets/native-environment.yaml` — native process inside a registered environment
2. Apply and watch:
   ```bash
   kubectl apply -f my-app.yaml
   kubectl -n NAMESPACE get pods -o wide -w
   ```
3. Confirm it is really running, not only scheduled. `READY 1/1` means the agent's local
   runtime is up and the HTTP readiness probe (if any) passed. The runtime's own message is in
   the Pod status:
   ```bash
   kubectl -n NAMESPACE get pod POD -o jsonpath='{.status.phase}{" | "}{.status.message}{"\n"}'
   kubectl -n NAMESPACE describe pod POD
   ```
4. Scale with `kubectl scale`; each replica gets its own local port. Update by changing the
   template, which recreates the Pods. Remove with `kubectl delete -f my-app.yaml` and verify the
   Pod is gone.

Use `strategy: {type: Recreate}` for anything that loads a large model onto the GPU. A rolling
update starts the new replica before stopping the old one, so two copies would hold memory at once.

## What the node supports (and does not)

Supported: one container per Pod; `command`, `args`, `workingDir`; `env` with literal values **and**
`valueFrom` (`fieldRef`: `metadata.name/namespace/uid/labels['k']/annotations['k']`, `spec.nodeName`,
`spec.serviceAccountName`, `status.hostIP`; `resourceFieldRef`: `requests|limits` `cpu|memory`; `secretKeyRef`,
`configMapKeyRef`), `envFrom` (with `prefix`, `optional`), and `$(VAR)` expansion in env, `command` and `args`;
one TCP `containerPort`, or several with the fixed-ports label; `readinessProbe`, `livenessProbe` and
`startupProbe` with `httpGet` (also HTTPS), `tcpSocket` or `exec`, aimed at a declared port (a failing liveness or
startup probe restarts the container under its `restartPolicy`); `lifecycle.postStart` and `lifecycle.preStop`
(`exec`, `httpGet`, `tcpSocket`, `sleep`); `terminationGracePeriodSeconds` (SIGTERM first, SIGKILL only after
the grace period; a `preStop` hook runs first and counts against it); `restartPolicy`; replicas; resource **requests**. For the **Docker** executor also `resources.limits`
(cpu and memory are enforced by Docker; exceeding memory ends as `OOMKilled`) and a `securityContext` subset:
`runAsUser`/`runAsGroup` (numeric), `runAsNonRoot` (needs `runAsUser`), `readOnlyRootFilesystem`,
`allowPrivilegeEscalation: false`, `capabilities` add/drop, `supplementalGroups`, `seccompProfile: RuntimeDefault`.
For **native**, `resources.limits` are accepted but **not enforced** (a Warning event says so), and only
`securityContext` values that are already true are accepted (the agent's own uid/gid, `runAsNonRoot` when the
agent is not root, `allowPrivilegeEscalation: false`).

Rejected or unsupported — the Pod stays `Pending` with reason `UnsupportedPodSpec`, the message lists every
offending field, and a Warning event says the same. Design around these instead of retrying:

- PVCs and other storage volumes (NFS, CSI), **any volume on a native Pod**, a projected `clusterTrustBundle`,
  `imagePullSecrets`, init/ephemeral containers,
  hostNetwork/hostPID/hostIPC for Docker (accepted for native), `hostPort`, `fieldRef status.podIP` (Mac Pods have
  no Pod IP)
- `grpc` probes, readiness gates, non-TCP ports, `privileged`, `fsGroup`, SELinux/Windows options, sysctls
- Secrets and ConfigMaps used as **environment variables** are read by the node when the container starts (the Node authorizer allows it only for
  objects the node's own Pods reference) and are not refreshed in a running program. A missing one leaves the Pod
  in `CreateContainerConfigError` until it exists. Resolved values are visible to anyone who can run `docker
  inspect` (Docker) or `ps eww` as the same macOS user (native); see docs/env-and-security.md before using real
  secrets.
- `automountServiceAccountToken` unset or `true` on a **native** Pod: the token is not mounted (Warning event
  `ServiceAccountTokenNotMounted`). Prefer `automountServiceAccountToken: false` there.

Networking is local only: there is no Pod IP, ClusterIP, Service endpoint, cluster DNS or
NetworkPolicy for these Pods. A workload is reachable on `127.0.0.1` on the Mac itself, so another
Pod in the cluster cannot call it by Service name. Do not promise cross-node connectivity.
`kubectl logs` (`--follow`, `--tail`, `--timestamps`, `--since`, `--since-time`, `--previous`), `exec` (with stdin,
`-t`, and the exit code), `cp`, `attach` and `port-forward` work **only when the node has its kubelet endpoint
enabled** (opt-in, with a serving certificate): check with `kubectl logs` on a Pod of the node first. Otherwise read
logs on the Mac (the app, or `maccluster-agent logs --uid POD_UID`). `exec` into a native program starts a new
process in that program's environment and working directory, not inside the program; `attach` to a native program
only follows its output. Log timestamps of native programs are accurate to about a second. `kubectl top` needs
metrics-server in the cluster; the node serves `/stats/summary` and `/metrics/resource` for it. See
docs/kubectl-and-kubelet.md.

## Volumes (Docker executor only)

Docker Pods can use `emptyDir`, `configMap`, `secret`, `downwardAPI`, `projected` (configMap, secret and
downwardAPI and `serviceAccountToken` sources) and `hostPath`. **Native Pods cannot have volumes** (they stay Pending with
`UnsupportedPodSpec`): pass settings with `env`/`envFrom` or read files by absolute path.

- **`emptyDir`** is a scratch directory owned by the Pod: it survives container restarts and is removed when the
  Pod finishes or is deleted. `medium: Memory` is a real tmpfs and `sizeLimit` is enforced for it only.
- **`configMap`/`secret`/`downwardAPI`/`projected`** become files (`items`, `path`, `mode`, `defaultMode`,
  `optional` work). Content is re-read about once a minute and replaced atomically, so a running container sees a
  changed ConfigMap without a restart (not through `subPath`). File mode bits are **not a security boundary** on
  Docker Desktop (the container sees everything as root); secret volumes are plain files in the agent's state
  directory, readable by the same macOS user.
- **`hostPath`** mounts an existing folder of the Mac into the container, and it needs **two things**:
  1. the Deployment asks for it:
     ```yaml
     containers:
       - name: app
         volumeMounts:
           - {name: models, mountPath: /models, readOnly: true}
     volumes:
       - name: models
         hostPath: {path: /Volumes/models, type: Directory}   # or DirectoryOrCreate / File / FileOrCreate
     ```
  2. the **Mac's operator** allows the path in `node-options.json` in the agent's state directory (then restarts
     the agent, which does not stop the workloads): `{"allowedHostPaths": ["/Volumes/models"]}`. The default is
     to refuse every hostPath. Paths are matched by whole path components after resolving symlinks
     (`/Volumes/models-evil` is not below `/Volumes/models`), and `DirectoryOrCreate` only creates inside an
     allowed path. Do not edit that file yourself unless the user asked you to.
  The path must also be inside Docker Desktop's file sharing (by default `/Users`, `/Volumes`, `/private`, `/tmp`).
  A hostPath's content is never cleaned up by the agent. Prefer `readOnly: true`.
- **ServiceAccount token:** only a Docker Pod with an explicit `automountServiceAccountToken: true` gets it (the API
  server's injected `kube-api-access-*` volume is then mounted like any projected volume, with `token`, `ca.crt`,
  `namespace`). The node requests it for that Pod (bound to the Pod's name and UID: it is refused as soon as the Pod
  is gone), renews it after ~80% of its lifetime, and sets `KUBERNETES_SERVICE_HOST`/`_PORT` to the control
  plane's real address (never overriding the Pod's own values; `apiServerAddress` in the node options sets it
  when the node reaches the API server through a loopback relay). The default ServiceAccount has no rights: create
  a Role and RoleBinding. A native Pod asking for it still runs, without the token and with a Warning event.
- A missing ConfigMap, Secret or hostPath keeps the Pod Pending with `FailedMount` and starts it automatically once
  the object exists. A hostPath outside the allow list is `HostPathNotAllowed`, and the message names the setting.
- Cleanup: volumes belong to the Pod, so a crash-looping container keeps its data; the agent removes a Pod's
  volume directory only after the Pod has finished (Succeeded/Failed, nothing running) or is gone.
- A PVC, `nfs` or `csi` volume is refused. For network storage, mount it on the Mac and use a `hostPath` below an
  allowed path.

Template: `examples/faq-docker-volumes.yaml` in the MacCluster repository.

Native specifics:

- `image` is only a label for humans; nothing is downloaded or converted. The program and its
  dependencies must already exist on the Mac and match its architecture (arm64).
- `command[0]` must be an absolute path, or a **bare command name** when an environment is
  selected (then it is looked up only in that environment's `bin`). A relative path with `/` is
  rejected. There is no implicit shell; use `command: [/bin/sh, -c, "…"]` when you need one.
- The agent picks a free local port and substitutes `$(MACCLUSTER_PORT)` in `args` and `env`; the
  program must listen on that port (bind `127.0.0.1`). The declared `containerPort` only tells the
  readiness probe which port to check.
- With an environment, `PATH` starts with its `bin`, its variables are applied before the Pod's
  own `env` (the Pod wins), `MACCLUSTER_ENV` is set, and `VIRTUAL_ENV` is set only for Python venvs.
- Stopping sends SIGTERM and then SIGKILL after a fixed delay; `terminationGracePeriodSeconds` is
  not honored exactly.

## Fixed ports (one or several)

Some services must listen on known ports, for example so other nodes can reach `<Mac LAN IP>:8100`
(there is no Pod IP or Service for this node). Declare them with a label on the Pod template:

```yaml
labels:
  maccluster.example.com/fixed-ports: "8100"        # one port
  maccluster.example.com/fixed-ports: "8100_9100"   # several ports, "_" separated
```

Rules (violations leave the Pod Pending with a message):

- native executor only; 1–8 distinct ports, each 1–65535, separated by `_` (label values cannot hold commas).
- every `containerPort` must be in the list; the `readinessProbe` port (number or named port) must be in the list.
- the agent allocates nothing: it sets `MACCLUSTER_PORT` (first port) and `MACCLUSTER_PORTS` (comma separated)
  and expands `$(MACCLUSTER_PORT)` and `$(MACCLUSTER_PORTS)` in `args`/`env`. Your program must bind exactly those ports.
- the probe checks the port named in `readinessProbe`, so a program with a hard-coded `--port 8100` becomes Ready.
- one replica per Mac per port. A busy port keeps the Pod Pending with `fixed port N is already in use` and
  is retried automatically; use `strategy: Recreate` so the old process releases the ports first.
- a program that binds `0.0.0.0` is reachable from the whole LAN, and there is no authentication or NetworkPolicy.
  Say so when you deploy one.

Template: `examples/faq-native-fixed-ports.yaml` in the MacCluster repository (the same shape as `assets/native-environment.yaml`
plus the label and extra `containerPort`s). **If the label seems to be ignored** (the Pod still reports an
allocated port, or a hard-coded port never becomes Ready), the Mac's agent predates this feature and needs to be
updated and restarted by its operator; restarting stops the native workloads running on that Mac, so ask first.

## Capacity: be conservative with GPU models

The node advertises fixed values (2 CPU, 8Gi memory, 20 Pods) that are not measured, and nothing
enforces Metal or unified-memory limits. A second large model can push a Mac into heavy memory
pressure without the scheduler noticing. Run one replica of a model unless the operator confirms
there is room, and never raise replicas of a model workload just to test scaling.

## When a Pod does not come up

| Symptom | Likely cause and fix |
| --- | --- |
| Pending, no node assigned, empty `status.message` | The scheduler could not place it: `nodeSelector` or toleration mismatch, wrong node name, an unreplaced placeholder such as `env-name`, or no node advertises the `env.maccluster.example.com/<name>` label. The reason is in `kubectl describe pod` events and the PodScheduled condition, not in `status.message`. |
| Pending, message "explicit … managed=true is required" | Add the `managed: "true"` label to the Pod template. |
| Pending, message `environment "x" is not defined on this node` | The Mac has no environment with that name, or its directory is gone. Ask the operator to register or repair it. |
| Pending, message says "not yet supported" or "not implemented" | The manifest uses something from the unsupported list above. Remove it. |
| Pending, `HostPathNotAllowed` | The `hostPath` is not below an entry of `allowedHostPaths` in the Mac's `node-options.json` (the message lists the current list). Ask the Mac's operator to allow it. |
| Pending, `FailedMount` | A ConfigMap, Secret or hostPath the volume needs does not exist (yet); create it and the Pod starts by itself. |
| Pending, message "unsupported executor" | `executor` label is neither `docker` nor `native`. |
| Running but never Ready ("connection refused" on `127.0.0.1:<big port>`) | The probe always targets the port the agent allocated, not `containerPort`. A program started with a hard-coded port (for example `--port 8100`) serves fine on that port while READY stays 0/1. Compare `lsof -nP -iTCP -sTCP:LISTEN -a -p <pid>` with the port in the agent's status. Fix: declare the port with the fixed-ports label (see "Fixed ports"), start the program on `$(MACCLUSTER_PORT)`, or drop the readiness probe. Also check the readiness path returns 2xx/3xx. |
| Docker Pod fails to start | Docker Desktop is not running on the Mac, or the image cannot be pulled there. |
| Repeated restarts | `restartPolicy: Always` with a crashing program; the agent backs off. Read logs and the restart count. |
| Pod "Terminating" for long | The agent stops the process first and then finishes the Pod; if the node is unreachable it cannot. Do not force-delete without confirming the process is stopped, because a native process could keep running. |

## Report back

After deploying, tell the user: the node name, namespace and Deployment name, the executor and
environment used, the Pod's phase and READY state, the local port if known, and anything you could
not verify (for example, that the service is reachable only on the Mac). If you created test
resources, say so and offer the delete command.
