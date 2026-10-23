#!/usr/bin/env python3
"""Test disposable local services through the same API used by the SwiftUI app."""
import argparse
import json
import os
import signal
import subprocess
import time

parser = argparse.ArgumentParser()
parser.add_argument('--state-dir', required=True)
args = parser.parse_args()
root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
base = [os.path.join(root, 'dist/maccluster-agent')]
def api(verb, data=None):
    p = subprocess.run(base + [verb, '--state-dir', args.state_dir],
                       input=json.dumps(data) if data is not None else None,
                       text=True, capture_output=True, check=True)
    return json.loads(p.stdout)
def wait_for(uid, predicate):
    deadline = time.monotonic() + 40
    while time.monotonic() < deadline:
        item = next((x for x in api('status')['localWorkloads'] if x['uid'] == uid), None)
        if item is not None and predicate(item):
            return item
        time.sleep(1)
    raise AssertionError(f'timed out for {uid}: {item}')

created = []
try:
    stamp = str(os.getpid())
    native = api('create', {'name': 'integration-native-'+stamp, 'executor':'native',
                           'command':['/bin/sleep'], 'args':['600'], 'restartPolicy':'Always'})
    created.append(native['uid'])
    assert native['phase'] == 'Running', native
    os.kill(native['pid'], signal.SIGKILL)
    recovered = wait_for(native['uid'], lambda x: x['phase']=='Running' and x['failureCount']==1 and x['restarts']>=1)
    assert recovered['pid'] != native['pid'] and 'Signal' in recovered['lastExitReason'], recovered
    print('PASS native SIGKILL detected; restart preserves failure history', flush=True)
    docker = api('create', {'name':'integration-docker-'+stamp,'image':'nginx:alpine','containerPort':80})
    created.append(docker['uid'])
    assert docker['phase']=='Running' and docker['executor']=='docker', docker
    subprocess.run(['docker','rm','-f',docker['containerID']],check=True,capture_output=True)
    removed = wait_for(docker['uid'],lambda x:x['phase']=='Failed' and x['failureCount']==1)
    assert removed['lastExitReason']=='ContainerRemoved', removed
    print('PASS externally removed Docker detected; default Never leaves failure visible',flush=True)
finally:
    for uid in created:
        api('delete',{'uid':uid})
    remaining = {x['uid'] for x in api('status')['localWorkloads']}
    assert not remaining.intersection(created)
    print('PASS disposable local definitions and runtime cleaned',flush=True)
