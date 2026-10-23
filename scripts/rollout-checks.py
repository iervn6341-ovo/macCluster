#!/usr/bin/env python3
"""Rolling updates and related Deployment behavior on the Mac node (docs/node-gap-roadmap.md, item 10).

controller-kinds.py checks that each controller kind works; this one measures what an update does to availability:
how many replicas stay Ready during a rollout, what a bad update does to the old replicas, rollback, pause/resume,
scaling and replacing a deleted Pod. Each scenario states what it EXPECTS and the script fails when reality differs.

  scripts/rollout-checks.py --kubeconfig KC --namespace NS --mac-node NAME [scenario ...]

Only the given namespace is touched; every Deployment it creates is deleted again.
"""
import argparse, json, subprocess, sys, time

ARGS = None
RUN = time.strftime('%H%M%S')


def kc(*a, inp=None, timeout=90):
    cmd = ['kubectl', '--kubeconfig', ARGS.kubeconfig, '--request-timeout=30s', '-n', ARGS.namespace] + list(a)
    p = subprocess.run(cmd, input=inp, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    return p.returncode, p.stdout.strip()


def jget(kind, name):
    c, o = kc('get', kind, name, '-o', 'json')
    return json.loads(o) if c == 0 else None


def wait(cond, seconds, every=1):
    end = time.time() + seconds
    while time.time() < end:
        v = cond()
        if v:
            return v
        time.sleep(every)
    return None


def pod_template(app, script, executor='native', ready_delay=None, env=None, image=None):
    labels = {'app': app, 'maccluster.example.com/managed': 'true', 'maccluster.example.com/test': 'rollout'}
    if executor == 'native':
        labels['maccluster.example.com/executor'] = 'native'
    c = {'name': 'c', 'image': image or ('native-placeholder' if executor == 'native' else 'nginx:alpine'),
         'command': ['/bin/sh', '-c', script], 'env': [{'name': k, 'value': v} for k, v in (env or {}).items()]}
    if ready_delay is not None:
        # Ready only after the file for this replica's own port exists (MACCLUSTER_PORT is unique per replica).
        c['command'] = ['/bin/sh', '-c', 'sleep %d; touch /tmp/rc-%s-$MACCLUSTER_PORT; %s' % (ready_delay, app, script)]
        c['readinessProbe'] = {'exec': {'command': ['/bin/sh', '-c', 'test -f /tmp/rc-%s-$MACCLUSTER_PORT' % app]}, 'periodSeconds': 1}
    if ready_delay == -1:  # never ready
        c['command'] = ['/bin/sh', '-c', script]
    spec = {'automountServiceAccountToken': False, 'terminationGracePeriodSeconds': 2,
            'nodeSelector': {'kubernetes.io/hostname': ARGS.mac_node},
            'tolerations': [{'key': k, 'operator': 'Exists', 'effect': 'NoSchedule'} for k in
                            ('maccluster.example.com/prototype', 'node.kubernetes.io/network-unavailable', 'node.kubernetes.io/memory-pressure')],
            'containers': [c]}
    return {'metadata': {'labels': labels}, 'spec': spec}


def deployment(name, replicas, template, strategy=None, deadline=None):
    d = {'apiVersion': 'apps/v1', 'kind': 'Deployment', 'metadata': {'name': name, 'namespace': ARGS.namespace},
         'spec': {'replicas': replicas, 'selector': {'matchLabels': {'app': template['metadata']['labels']['app']}}, 'template': template}}
    if strategy:
        d['spec']['strategy'] = strategy
    if deadline:
        d['spec']['progressDeadlineSeconds'] = deadline
    return d


def apply(obj):
    c, o = kc('apply', '-f', '-', inp=json.dumps(obj))
    if c:
        raise RuntimeError(o)


def counts(name):
    st = (jget('deployment', name) or {}).get('status', {})
    return {'ready': st.get('readyReplicas', 0), 'available': st.get('availableReplicas', 0), 'total': st.get('replicas', 0),
            'updated': st.get('updatedReplicas', 0)}


def settled(name, n):
    c = counts(name)
    return c['ready'] == n and c['total'] == n and c['updated'] == n


def track_rollout(name, n, seconds):
    """Sample once a second until the rollout is complete; return (completed, min ready, max total pods)."""
    lo, hi, done = n, 0, False
    end = time.time() + seconds
    while time.time() < end:
        c = counts(name)
        lo, hi = min(lo, c['ready']), max(hi, c['total'])
        if settled(name, n):
            done = True
            break
        time.sleep(1)
    return done, lo, hi


def live_pods(name):
    """Pods of the Deployment that are not being deleted (a Docker Pod lingers as Terminating while it stops)."""
    c, o = kc('get', 'pods', '-l', 'app=' + name, '-o', 'json')
    return [p for p in (json.loads(o)['items'] if c == 0 else []) if not p['metadata'].get('deletionTimestamp')]


def cleanup(name):
    kc('delete', 'deployment', name, '--wait=false', '--ignore-not-found')
    time.sleep(4)


def start(name, n, tpl, **kw):
    apply(deployment(name, n, tpl, **kw))
    return wait(lambda: settled(name, n), 90, 2)


# ---- scenarios

def s_native_zero_downtime_update():
    name = 'ro-zd-' + RUN
    if not start(name, 3, pod_template(name, 'sleep 3600', ready_delay=4, env={'V': '1'})):
        cleanup(name)
        return 'NEVER-READY', ''
    apply(deployment(name, 3, pod_template(name, 'sleep 3600', ready_delay=4, env={'V': '2'})))
    done, lo, hi = track_rollout(name, 3, 120)
    cleanup(name)
    # Default strategy: maxUnavailable 25% of 3 rounds down to 0, maxSurge rounds up to 1.
    if not done:
        return 'NOT-COMPLETED', 'min ready %d, max pods %d' % (lo, hi)
    return ('ZERO-DOWNTIME' if lo >= 3 and hi <= 4 else 'DISRUPTED'), 'min ready %d, max pods %d' % (lo, hi)


def bad_update(name, script, ready_delay, then_undo=True):
    if not start(name, 3, pod_template(name, 'sleep 3600', ready_delay=2, env={'V': '1'}), deadline=30):
        cleanup(name)
        return 'NEVER-READY', ''
    apply(deployment(name, 3, pod_template(name, script, ready_delay=ready_delay, env={'V': 'bad'}), deadline=30))
    lowest = 3
    for _ in range(30):  # watch the old replicas for 30 s while the bad update tries to proceed
        lowest = min(lowest, counts(name)['ready'])
        time.sleep(1)
    c, status = kc('rollout', 'status', 'deployment/' + name, '--timeout=20s')
    stalled = c != 0
    kc('rollout', 'undo', 'deployment/' + name)
    recovered = wait(lambda: settled(name, 3), 90, 2)
    cleanup(name)
    detail = 'lowest Ready during the bad update=%d (stall reported=%s), after undo settled=%s' % (lowest, stalled, bool(recovered))
    if stalled and lowest >= 3 and recovered:
        return 'OLD-KEPT-THEN-ROLLED-BACK', detail
    return 'UNEXPECTED', detail


def s_native_update_never_ready():
    return bad_update('ro-never-' + RUN, 'sleep 3600', -1)


def s_native_update_crashes():
    return bad_update('ro-crash-' + RUN, 'echo crash; exit 1', None)


def s_native_old_stays_until_new_ready():
    name = 'ro-order-' + RUN
    if not start(name, 1, pod_template(name, 'sleep 3600', ready_delay=1, env={'V': '1'})):
        cleanup(name)
        return 'NEVER-READY', ''
    old = [p['metadata']['name'] for p in json.loads(kc('get', 'pods', '-l', 'app=' + name, '-o', 'json')[1])['items']]
    apply(deployment(name, 1, pod_template(name, 'sleep 3600', ready_delay=12, env={'V': '2'})))
    time.sleep(7)
    # Mid-rollout: the new Pod is not ready yet (12 s delay), so the old Pod must still be there and Ready.
    items = json.loads(kc('get', 'pods', '-l', 'app=' + name, '-o', 'json')[1])['items']
    byname = {p['metadata']['name']: p for p in items}
    old_ready = old[0] in byname and byname[old[0]]['status'].get('phase') == 'Running' and \
        byname[old[0]].get('metadata', {}).get('deletionTimestamp') is None
    done = wait(lambda: settled(name, 1), 60, 2)
    cleanup(name)
    return ('OLD-KEPT' if old_ready and done else 'OLD-REMOVED-EARLY'), 'old kept mid-rollout=%s, completed=%s' % (old_ready, bool(done))


def s_docker_image_update_and_rollback():
    name = 'ro-dk-' + RUN
    tpl = pod_template(name, 'nginx -g "daemon off;"', 'docker', image='nginx:alpine')
    tpl['spec']['containers'][0].pop('command')
    if not start(name, 2, tpl):
        cleanup(name)
        return 'NEVER-READY', ''
    tpl2 = pod_template(name, '', 'docker', image='nginx:stable-alpine')
    tpl2['spec']['containers'][0].pop('command')
    apply(deployment(name, 2, tpl2))
    done, lo, hi = track_rollout(name, 2, 180)
    wait(lambda: len(live_pods(name)) == 2 and {p['spec']['containers'][0]['image'] for p in live_pods(name)} == {'nginx:stable-alpine'}, 30, 2)
    imgs = {p['spec']['containers'][0]['image'] for p in live_pods(name)}
    kc('rollout', 'undo', 'deployment/' + name)
    back = wait(lambda: settled(name, 2) and {p['spec']['containers'][0]['image'] for p in live_pods(name)} == {'nginx:alpine'}, 120, 2)
    imgs_back = {p['spec']['containers'][0]['image'] for p in live_pods(name)}
    cleanup(name)
    ok = done and imgs == {'nginx:stable-alpine'} and back and imgs_back == {'nginx:alpine'}
    return ('UPDATED-AND-ROLLED-BACK' if ok else 'FAILED'), 'updated=%s images=%s; rolled back=%s images=%s; min ready %d' % (bool(done), sorted(imgs), bool(back), sorted(imgs_back), lo)


def s_scale_up_and_down():
    name = 'ro-scale-' + RUN
    if not start(name, 3, pod_template(name, 'sleep 3600', ready_delay=1)):
        cleanup(name)
        return 'NEVER-READY', ''
    seen = [3]
    for n in (1, 4):
        kc('scale', 'deployment/' + name, '--replicas=%d' % n)
        wait(lambda: settled(name, n), 60, 2)
        seen.append(counts(name)['ready'])
    cleanup(name)
    return ('SCALED' if seen == [3, 1, 4] else 'WRONG-COUNTS'), 'ready after 3->1->4: %s' % seen


def s_pause_and_resume():
    name = 'ro-pause-' + RUN
    if not start(name, 2, pod_template(name, 'sleep 3600', ready_delay=1, env={'V': '1'})):
        cleanup(name)
        return 'NEVER-READY', ''
    before = {p['metadata']['name'] for p in live_pods(name)}
    kc('rollout', 'pause', 'deployment/' + name)
    apply(deployment(name, 2, pod_template(name, 'sleep 3600', ready_delay=1, env={'V': '2'})))
    time.sleep(10)
    during = {p['metadata']['name'] for p in live_pods(name)}
    kc('rollout', 'resume', 'deployment/' + name)
    # Done when every old Pod is gone and two new ones are Ready.
    done = wait(lambda: settled(name, 2) and not ({p['metadata']['name'] for p in live_pods(name)} & before), 90, 2)
    after = {p['metadata']['name'] for p in live_pods(name)}
    cleanup(name)
    ok = during == before and done and not (after & before)
    return ('HELD-THEN-ROLLED' if ok else 'UNEXPECTED'), 'unchanged while paused=%s, replaced after resume=%s' % (during == before, not (after & before))


def s_deleted_pod_is_replaced():
    name = 'ro-del-' + RUN
    if not start(name, 2, pod_template(name, 'sleep 3600', ready_delay=1)):
        cleanup(name)
        return 'NEVER-READY', ''
    victim = json.loads(kc('get', 'pods', '-l', 'app=' + name, '-o', 'json')[1])['items'][0]['metadata']['name']
    t0 = time.time()
    kc('delete', 'pod', victim, '--wait=false')
    ok = wait(lambda: settled(name, 2) and victim not in kc('get', 'pods', '-l', 'app=' + name, '-o', 'name')[1], 60, 1)
    took = time.time() - t0
    cleanup(name)
    return ('REPLACED' if ok else 'NOT-REPLACED'), 'back to 2 Ready after %.0f s' % took


SCENARIOS = {
    'native_zero_downtime_update': (s_native_zero_downtime_update, 'ZERO-DOWNTIME'),
    'native_update_never_ready': (s_native_update_never_ready, 'OLD-KEPT-THEN-ROLLED-BACK'),
    'native_update_crashes': (s_native_update_crashes, 'OLD-KEPT-THEN-ROLLED-BACK'),
    'native_old_stays_until_new_ready': (s_native_old_stays_until_new_ready, 'OLD-KEPT'),
    'docker_image_update_and_rollback': (s_docker_image_update_and_rollback, 'UPDATED-AND-ROLLED-BACK'),
    'scale_up_and_down': (s_scale_up_and_down, 'SCALED'),
    'pause_and_resume': (s_pause_and_resume, 'HELD-THEN-ROLLED'),
    'deleted_pod_is_replaced': (s_deleted_pod_is_replaced, 'REPLACED'),
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
        print('%s %-34s expected=%-26s got=%-26s %3ds  %s' % ('ok' if ok else 'XX', n, want, got, time.time() - t0, detail), flush=True)
    print('\n%d of %d scenarios as expected' % (len(names) - bad, len(names)))
    sys.exit(1 if bad else 0)


main()
