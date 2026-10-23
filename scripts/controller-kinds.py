#!/usr/bin/env python3
"""How the Kubernetes workload controllers behave on the Mac node: rolling updates of Deployments with fixed ports,
Jobs, CronJobs, StatefulSets and DaemonSets. It records what happens, states what is EXPECTED, and fails when
reality differs (docs/node-gap-roadmap.md, item 10).

  scripts/controller-kinds.py --kubeconfig KC --namespace NS --mac-node NAME [scenario ...]

Only the given namespace is touched, and every object this script creates is deleted again.
"""
import argparse, json, subprocess, sys, time

ARGS = None
RUN = time.strftime('%H%M%S')


def kc(*a, inp=None, timeout=60):
    cmd = ['kubectl', '--kubeconfig', ARGS.kubeconfig, '--request-timeout=30s', '-n', ARGS.namespace] + list(a)
    p = subprocess.run(cmd, input=inp, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    return p.returncode, p.stdout.strip()


def jget(kind, name):
    c, o = kc('get', kind, name, '-o', 'json')
    return json.loads(o) if c == 0 else None


def apply(obj):
    c, o = kc('apply', '-f', '-', inp=json.dumps(obj))
    if c:
        raise RuntimeError(o)


def template(app, script, executor='native', fixed=None, restart=None, extra_labels=None):
    labels = {'app': app, 'maccluster.example.com/managed': 'true', 'maccluster.example.com/test': 'controller-kinds'}
    if executor == 'native':
        labels['maccluster.example.com/executor'] = 'native'
    if fixed:
        labels['maccluster.example.com/fixed-ports'] = str(fixed)
    labels.update(extra_labels or {})
    spec = {'automountServiceAccountToken': False, 'terminationGracePeriodSeconds': 2,
            'nodeSelector': {'kubernetes.io/hostname': ARGS.mac_node},
            'tolerations': [{'key': k, 'operator': 'Exists', 'effect': 'NoSchedule'} for k in
                            ('maccluster.example.com/prototype', 'node.kubernetes.io/network-unavailable', 'node.kubernetes.io/memory-pressure')],
            'containers': [{'name': 'c', 'image': 'native-placeholder' if executor == 'native' else 'nginx:alpine',
                            'command': ['/bin/sh', '-c', script]}]}
    if restart:
        spec['restartPolicy'] = restart
    return {'metadata': {'labels': labels}, 'spec': spec}


def pods(app):
    c, o = kc('get', 'pods', '-l', 'app=' + app, '-o', 'json')
    return json.loads(o)['items'] if c == 0 else []


def wait(cond, seconds, every=2):
    end = time.time() + seconds
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(every)
    return None


def cleanup(kinds_names):
    for kind, name in kinds_names:
        kc('delete', kind, name, '--wait=false', '--ignore-not-found')
    time.sleep(3)


# ---- scenarios: each returns (verdict, detail)

def deployment_update(name, strategy, fixed):
    port = 18700 + (hash(name) % 90)
    d = {'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': name, 'namespace': ARGS.namespace},
         'spec': {'replicas': 1, 'selector': {'matchLabels': {'app': name}}, 'template': template(name, 'echo v1; sleep 3600', fixed=port if fixed else None)}}
    if strategy:
        d['spec']['strategy'] = strategy
    apply(d)
    if not wait(lambda: [p for p in pods(name) if p['status'].get('phase') == 'Running'], 40):
        cleanup([('deployment', name)])
        return 'NEVER-STARTED', ''
    d['spec']['template']['spec']['containers'][0]['command'] = ['/bin/sh', '-c', 'echo v2; sleep 3600']
    apply(d)

    def rolled():
        obj = jget('deployment', name)
        st = obj['status']
        return st.get('updatedReplicas') == 1 and st.get('replicas') == 1 and st.get('availableReplicas') == 1
    done = wait(rolled, 70, 3)
    ps = pods(name)
    detail = ['%s=%s' % (p['metadata']['name'][-5:], p['status'].get('phase')) for p in ps]
    msgs = [p['status'].get('containerStatuses', [{}])[0].get('state', {}).get('waiting', {}).get('message', '') for p in ps]
    cleanup([('deployment', name)])
    if done:
        return 'COMPLETED', ' '.join(detail)
    busy = [m for m in msgs if m]
    return 'STALLED', ' '.join(detail) + (' | ' + busy[0][:90] if busy else '')


def s_rolling_default_fixed_port():
    return deployment_update('ck-roll-default-' + RUN, None, True)


def s_rolling_surge0_fixed_port():
    return deployment_update('ck-roll-surge0-' + RUN, {'type': 'RollingUpdate', 'rollingUpdate': {'maxSurge': 0, 'maxUnavailable': 1}}, True)


def s_recreate_fixed_port():
    return deployment_update('ck-recreate-' + RUN, {'type': 'Recreate'}, True)


def s_rolling_default_dynamic_port():
    return deployment_update('ck-roll-dyn-' + RUN, None, False)


def job(name, script, executor, restart, backoff=1, completions=1):
    apply({'apiVersion': 'batch/v1', 'kind': 'Job', 'metadata': {'name': name, 'namespace': ARGS.namespace},
           'spec': {'backoffLimit': backoff, 'completions': completions, 'parallelism': completions,
                    'template': template(name, script, executor, restart=restart)}})

    def finished():
        st = (jget('job', name) or {}).get('status', {})
        if st.get('succeeded', 0) >= completions:
            return 'COMPLETE'
        for cnd in st.get('conditions', []):
            if cnd['type'] == 'Failed' and cnd['status'] == 'True':
                return 'FAILED'
    res = wait(finished, 100, 3) or 'RUNNING'
    st = (jget('job', name) or {}).get('status', {})
    detail = 'succeeded=%s failed=%s' % (st.get('succeeded', 0), st.get('failed', 0))
    cleanup([('job', name)])
    return res, detail


def s_job_native_success():
    return job('ck-job-ok-' + RUN, 'echo done', 'native', 'Never')


def s_job_native_failure_backoff():
    return job('ck-job-fail-' + RUN, 'echo boom; exit 3', 'native', 'Never', backoff=1)


def s_job_native_onfailure_retry():
    # OnFailure restarts the program inside the same Pod until the Job gives up.
    return job('ck-job-onf-' + RUN, 'echo boom; exit 3', 'native', 'OnFailure', backoff=1)


def s_job_native_parallel():
    return job('ck-job-par-' + RUN, 'sleep 2; echo part', 'native', 'Never', completions=3)


def s_job_docker_success():
    return job('ck-job-dk-' + RUN, 'echo done', 'docker', 'Never')


def s_cronjob_creates_jobs():
    name = 'ck-cron-' + RUN
    apply({'apiVersion': 'batch/v1', 'kind': 'CronJob', 'metadata': {'name': name, 'namespace': ARGS.namespace},
           'spec': {'schedule': '* * * * *', 'successfulJobsHistoryLimit': 1, 'jobTemplate': {'spec': {'template': template(name, 'echo cron', 'native', restart='Never')}}}})

    def ran():
        c, o = kc('get', 'jobs', '-o', 'json')
        for j in json.loads(o)['items'] if c == 0 else []:
            if j['metadata']['name'].startswith(name) and j.get('status', {}).get('succeeded', 0) >= 1:
                return j['metadata']['name']
    res = wait(ran, 130, 5)
    cleanup([('cronjob', name)])
    c, o = kc('get', 'jobs', '-o', 'name')
    for j in o.split():
        if name in j:
            kc('delete', j, '--wait=false')
    return ('COMPLETE', res) if res else ('NO-JOB', '')


def s_statefulset_two_replicas():
    name = 'ck-sts-' + RUN
    apply({'apiVersion': 'v1', 'kind': 'Service', 'metadata': {'name': name, 'namespace': ARGS.namespace}, 'spec': {'clusterIP': 'None', 'selector': {'app': name}}})
    apply({'apiVersion': 'apps/v1', 'kind': 'StatefulSet', 'metadata': {'name': name, 'namespace': ARGS.namespace},
           'spec': {'replicas': 2, 'serviceName': name, 'selector': {'matchLabels': {'app': name}},
                    'template': template(name, 'echo $HOSTNAME; sleep 3600')}})
    ok = wait(lambda: len([p for p in pods(name) if p['status'].get('phase') == 'Running']) == 2, 60)
    names = sorted(p['metadata']['name'].replace(name, 'sts') for p in pods(name))
    cleanup([('statefulset', name), ('service', name)])
    return ('READY', ' '.join(names)) if ok else ('NOT-READY', ' '.join(names))


def s_daemonset_one_pod():
    name = 'ck-ds-' + RUN
    apply({'apiVersion': 'apps/v1', 'kind': 'DaemonSet', 'metadata': {'name': name, 'namespace': ARGS.namespace},
           'spec': {'selector': {'matchLabels': {'app': name}}, 'template': template(name, 'sleep 3600')}})
    ok = wait(lambda: len([p for p in pods(name) if p['status'].get('phase') == 'Running']) == 1, 60)
    ds = (jget('daemonset', name) or {}).get('status', {})
    cleanup([('daemonset', name)])
    return ('READY' if ok else 'NOT-READY'), 'desired=%s ready=%s' % (ds.get('desiredNumberScheduled'), ds.get('numberReady'))


# What each scenario is expected to do. STALLED is the documented limitation of fixed ports (docs/fixed-ports.md):
# the new Pod waits for the old one's port, and a default rolling update never lets the old one go.
SCENARIOS = {
    'rolling_default_fixed_port': (s_rolling_default_fixed_port, 'STALLED'),
    'rolling_surge0_fixed_port': (s_rolling_surge0_fixed_port, 'COMPLETED'),
    'recreate_fixed_port': (s_recreate_fixed_port, 'COMPLETED'),
    'rolling_default_dynamic_port': (s_rolling_default_dynamic_port, 'COMPLETED'),
    'job_native_success': (s_job_native_success, 'COMPLETE'),
    'job_native_failure_backoff': (s_job_native_failure_backoff, 'FAILED'),
    'job_native_onfailure_retry': (s_job_native_onfailure_retry, 'FAILED'),
    'job_native_parallel': (s_job_native_parallel, 'COMPLETE'),
    'job_docker_success': (s_job_docker_success, 'COMPLETE'),
    'cronjob_creates_jobs': (s_cronjob_creates_jobs, 'COMPLETE'),
    'statefulset_two_replicas': (s_statefulset_two_replicas, 'READY'),
    'daemonset_one_pod': (s_daemonset_one_pod, 'READY'),
}


def main():
    global ARGS
    ap = argparse.ArgumentParser()
    ap.add_argument('--kubeconfig', required=True)
    ap.add_argument('--namespace', required=True)
    ap.add_argument('--mac-node', required=True)
    ap.add_argument('scenarios', nargs='*')
    ARGS = ap.parse_args()
    names = ARGS.scenarios or list(SCENARIOS)
    bad = 0
    for n in names:
        fn, want = SCENARIOS[n]
        t0 = time.time()
        try:
            got, detail = fn()
        except Exception as e:
            got, detail = 'ERROR', repr(e)[:160]
        ok = got == want
        bad += not ok
        print('%s %-30s expected=%-10s got=%-10s %3ds  %s' % ('ok' if ok else 'XX', n, want, got, time.time() - t0, detail), flush=True)
    print('\n%d of %d scenarios as expected' % (len(names) - bad, len(names)))
    sys.exit(1 if bad else 0)


main()
