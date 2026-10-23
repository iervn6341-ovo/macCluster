#!/usr/bin/env python3
"""Run opt-in integration tests against only examples/smoke.yaml resources.

Usage: python3 scripts/integration.py --kubeconfig PATH --state-dir PATH
HTTPS_PROXY can point to a pre-existing tunnel. No secrets are printed.
Leaves both smoke Deployments at one replica for manual inspection.
"""
import argparse
import json
import pathlib
import subprocess
import time
import urllib.request

parser = argparse.ArgumentParser()
parser.add_argument("--kubeconfig", required=True)
parser.add_argument("--state-dir", required=True)
parser.add_argument("--cluster-id", help="Scope runtime checks to one cluster profile")
parser.add_argument("--namespace", default="maccluster-test")
parser.add_argument("--node-name", default="maccluster-node", help="Node this Mac registered as; replaces the example node name in examples/smoke.yaml")
parser.add_argument("--fresh-namespace", action="store_true", help="Create namespace exclusively; refuse if it exists")
args = parser.parse_args()
root = pathlib.Path(__file__).resolve().parents[1]
kubectl = ["kubectl", "--kubeconfig", args.kubeconfig, "-n", args.namespace]
agent = [str(root / "dist/maccluster-agent"), "status", "--state-dir", args.state_dir]

def run(command):
    return subprocess.check_output(command, text=True, timeout=30)

def snapshot():
    state = json.loads(run(agent))
    if args.cluster_id:
        matches = [c for c in state.get("clusters", []) if c["id"] == args.cluster_id]
        if len(matches) != 1:
            raise RuntimeError("Requested cluster profile unavailable")
        return matches[0]
    if len(state.get("clusters", [])) > 1:
        raise RuntimeError("Multiple clusters present; specify --cluster-id")
    return state

def verify(replicas):
    deadline = time.monotonic() + 90
    last = ""
    while time.monotonic() < deadline:
        state = snapshot()
        workloads = [w for w in state["workloads"] if w["namespace"] == args.namespace and (w["name"].startswith("nginx-") or w["name"].startswith("native-http-"))]
        pods = json.loads(run(kubectl + ["get", "pods", "-o", "json"]))["items"]
        pods = [p for p in pods if p["metadata"]["labels"].get("app") in ("maccluster-nginx", "maccluster-native-http")]
        active = [p for p in pods if not p["metadata"].get("deletionTimestamp")]
        good = len(workloads) == replicas * 2 and len(active) == replicas * 2
        good = good and all(sum(w["executor"] == kind for w in workloads) == replicas for kind in ("docker", "native"))
        good = good and all(any(c["type"] == "Ready" and c["status"] == "True" for c in p["status"].get("conditions", [])) for p in active)
        good = good and all(w["phase"] == "Running" for w in workloads)
        if good:
            ports = [w["port"] for w in workloads]
            assert len(set(ports)) == len(ports), "Replica ports collide"
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
            for port in ports:
                with opener.open(f"http://127.0.0.1:{port}/", timeout=3) as response:
                    assert response.status == 200
            print(f"PASS replicas={replicas}: Kubernetes Ready, exact runtime counts, distinct ports, HTTP 200")
            return
        last = [(w["name"], w["phase"], w["message"]) for w in workloads]
        time.sleep(2)
    raise RuntimeError(f"Replica verification timed out: {last}")

smoke = (root / "examples/smoke.yaml").read_text().replace("maccluster-node", args.node_name)
if args.fresh_namespace:
    print(run(kubectl + ["create", "namespace", args.namespace]).strip())
    manifest = smoke.split("---", 1)[1]
    manifest = manifest.replace("namespace: maccluster-test", "namespace: " + args.namespace)
    subprocess.run(kubectl + ["apply", "-f", "-"], input=manifest, text=True, check=True)
else:
    if args.namespace != "maccluster-test":
        raise RuntimeError("Custom namespace requires --fresh-namespace")
    subprocess.run(kubectl + ["apply", "-f", "-"], input=smoke, text=True, check=True)
for replicas in (1, 2, 1, 0, 1):
    print(run(kubectl + ["scale", "deployment/nginx", "deployment/native-http", f"--replicas={replicas}"]).strip())
    verify(replicas)
