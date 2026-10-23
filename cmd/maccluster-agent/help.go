package main

import (
	"fmt"
	"io"
)

const cliHelp = `MacCluster CLI — Manage clusters, Docker containers, and native services on Mac

Usage:
  maccluster-agent <command> [options]
  maccluster-agent <command> --help
  maccluster-agent help [command]

Commands:
  service         Install, start, stop, restart, or inspect the background agent
  status          Show clusters, workloads, and runtime events as JSON
  join            Join a cluster using JSON from stdin
  configure       Set a cluster's management kubeconfig using JSON from stdin
  leave           Leave a cluster and stop its workloads using JSON from stdin
  env             Register, list, or remove local execution environments
  create          Create a local Docker or native service using JSON from stdin
  delete          Delete a workload using JSON from stdin
  logs            Show workload logs (--uid UID [--cluster-id ID])
  inventory       List external Docker containers
  docker-inspect  Inspect a container using JSON from stdin
  docker-stats    Show container resource usage using JSON from stdin
  docker-logs     Show Docker logs using JSON from stdin
  docker-control  Start or stop an external Docker container using JSON from stdin
  docker-login    Log in to an image registry using JSON from stdin
  docker-logout   Log out of an image registry using JSON from stdin
  docker-logins   List registry login records
  settings        Update agent settings using JSON from stdin
  serve           Run the agent in the foreground; do not start a duplicate agent
  drain           Stop managed workloads (destructive; not a status query)
  smoke           Run local smoke tests

Advanced commands (each supports --help):
  run-node, egress-proxy, network-controller, network-controller-manifests

Common options:
  --state-dir PATH
      Default: ~/Library/Application Support/MacCluster
      The default path shares state with the GUI.

env accesses the environment registry directly. Management commands such as
status, join, and create require a running agent.
env add does not create a virtualenv or install packages; prepare the directory first.
`

var commandHelp = map[string]string{
	"service": `service <install|start|stop|restart|status|config>
  install [--state-dir PATH]  Install this executable as the login background agent.
                            Stop a loaded service before replacing its configuration.
  start                     Start the installed LaunchAgent (no GUI required).
  stop                      Unload it; keep workloads running, prevent immediate respawn.
  restart                   Unload and start the installed LaunchAgent.
  status                    Show launchd service details (not workload health).
  config [--state-dir PATH]  Print LaunchAgent plist without installing it.

Uses the current user's com.maccluster.agent job, shared with the GUI.
install does not start the service; keep the executable at its installed path.
SIGKILL (kill -9) of the agent suppresses automatic restart until service start
or the next login. Other agent exits still trigger restart.
stop retains the plist: the service starts again at next login.
For foreground use, run serve and stop it with Ctrl-C.`,
	"serve": `serve [--state-dir PATH]
Run the agent in the foreground. Do not start another agent with the same state directory.`,
	"inventory": `inventory
List external containers in the current Docker context as JSON.`,
	"docker-inspect": `docker-inspect
Stdin JSON: {"containerID":"CONTAINER_ID"}`,
	"docker-stats": `docker-stats
Stdin JSON: {"containerID":"CONTAINER_ID"}`,
	"docker-logs": `docker-logs
Stdin JSON: {"containerID":"CONTAINER_ID"}`,
	"docker-control": `docker-control
Stdin JSON: {"containerID":"CONTAINER_ID","action":"start"}
action must be start or stop. Only external Docker containers are supported.`,
	"docker-login": `docker-login
Stdin JSON: {"registry":"REGISTRY","username":"USERNAME","password":"PASSWORD"}
An empty registry selects Docker Hub. Do not put passwords in command-line arguments.`,
	"docker-logout": `docker-logout
Stdin JSON: {"registry":"REGISTRY"}`,
	"docker-logins": `docker-logins
List Docker registry login records.`,
	"settings": `settings [--state-dir PATH]
Stdin JSON: {"keepAwake":"when-workloads"}
keepAwake must be when-workloads, always, or never.`,
	"drain": `drain [--state-dir PATH]
Stop managed workloads and shut down the agent. launchd may restart the agent;
this command is not a status query.`,
	"smoke": `smoke [--state-dir PATH]
Run local smoke tests. This creates test resources.`,
	"netcheck": `netcheck [--state-dir PATH]
Ask the running agent whether it can reach each joined control plane (a direct connection, the proxy named in the
node kubeconfig, the node certificate) and print the result as JSON. The checks run inside the agent process because
macOS grants the Local Network privacy setting per program.`,
	"status": `status [--state-dir PATH]
Output clusters, workloads, and events as JSON.`,
	"join": `join [--state-dir PATH]
Read JSON from stdin. Parse kubeadm join arguments without executing the shell text.
Example (replace the placeholders):
  {"name":"My cluster","nodeName":"my-mac","command":"kubeadm join HOST:6443 --token TOKEN --discovery-token-ca-cert-hash sha256:HASH"}
Optional fields: proxyURL, managementKubeconfig (absolute path to an admin kubeconfig).
A bootstrap token does not grant administrator permissions.`,
	"env": `env <add|list|shell|remove> [options]
  add --name NAME --root /absolute/path [--type link|dir]
      [--bin-dir PATH] [--workdir PATH] [--extra-path PATH]...
      [--set-env KEY=VALUE]... [--state-dir PATH]
      Register or overwrite an existing environment; does not create a virtualenv
      or install packages.
  list [--json] [--state-dir PATH]
  shell --name NAME [--state-dir PATH]
      Output shell launch settings as JSON; does not open an interactive shell.
  remove --name NAME [--state-dir PATH]
      Remove only the registry entry, not the environment directory.

Examples:
  python3 -m venv "$HOME/.venvs/my-service"
  maccluster-agent env add --name my-service --root "$HOME/.venvs/my-service"
  maccluster-agent env list`,
	"create": `create [--state-dir PATH]
Read a local service definition as JSON from stdin. Example:
  {"name":"nginx","executor":"docker","image":"nginx:alpine","containerPort":80,"restartPolicy":"Always"}
Native process example:
  {"name":"http","executor":"native","environment":"my-service","command":["python"],"args":["-m","http.server","$(MACCLUSTER_PORT)"]}
Other fields: env, workingDir, fixedPorts, restartPolicy (Never, Always, or OnFailure).
This creates a local service. Use kubectl to create Kubernetes Deployments.`,
	"logs": `logs --uid UID [--cluster-id ID] [--state-dir PATH]
Omit cluster-id for local service logs. Obtain cluster IDs and workload UIDs from status.`,
	"configure": `configure [--state-dir PATH]
Stdin JSON: {"clusterID":"ID","managementKubeconfig":"/absolute/path/admin.kubeconfig"}`,
	"leave": `leave [--state-dir PATH]
Stdin JSON: {"clusterID":"ID"}
Stop this cluster's workloads on the Mac and remove its Node and local connection
settings. Settings are retained on failure so you can retry.`,
	"delete": `delete [--state-dir PATH]
Stdin JSON: {"uid":"UID","clusterID":"","mode":"workload"}
An empty clusterID selects a local service. For cluster workloads, mode can be pod
or workload. A deleted Pod may be recreated by its controller; workload deletes
the owning controller.`,
}

func writeCommandHelp(w io.Writer, command string) bool {
	text, ok := commandHelp[command]
	if ok {
		fmt.Fprintln(w, "Usage: maccluster-agent "+text)
	}
	return ok
}
