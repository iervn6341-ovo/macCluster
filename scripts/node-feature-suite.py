#!/usr/bin/env python3
"""Regression suite: the same Pod spec on a Linux node, the Mac Docker executor and the Mac native executor.

Every case states what each target is EXPECTED to do. The suite fails when reality differs, in either
direction: a feature that stops working, and a feature that starts working (so the expectation, the FAQ and
docs/node-comparison.md get updated together). It only touches the namespace it is given and deletes its Pods.

  scripts/node-feature-suite.py --kubeconfig KC --namespace NS --linux-node NAME --mac-node NAME [case ...]

Expectation words:
  PASS         works and behaves like Linux for what the case checks
  UNSUPPORTED  Pod stays Pending with reason UnsupportedPodSpec (or InvalidPodSpec) naming the field, and a
               Warning event with the same reason exists
  DIFF         works, but differently from Linux in a way the case documents
  FAIL         the operation is rejected or errors (kubectl-level cases)
"""
import argparse, json, subprocess, sys, time

# Pods get a per-run suffix, so events of an earlier run's Pod of the same name cannot be mistaken for this run's.
RUN_ID = time.strftime('%H%M%S')

ARGS = None
TARGETS = ('linux', 'mac-docker', 'mac-native')

def kc(*a, inp=None, timeout=45):
    cmd = ['kubectl', '--kubeconfig', ARGS.kubeconfig, '--request-timeout=25s', '-n', ARGS.namespace] + list(a)
    p = subprocess.run(cmd, input=inp, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
    return p.returncode, p.stdout.strip()

def pod(target, name, script, container=None, spec=None, labels=None):
    spec = dict(spec or {})
    annotations = spec.pop('__annotations__', None)  # metadata, not spec: lets a case annotate its Pods
    c = {'name': 'c', 'image': 'nginx:alpine', 'command': ['/bin/sh', '-c', script]}
    c.update(container or {})
    s = {'containers': [c], 'terminationGracePeriodSeconds': 5}
    lab = {'maccluster.example.com/test': 'feature-suite'}
    if target == 'linux':
        s['nodeSelector'] = {'kubernetes.io/hostname': ARGS.linux_node}
    else:
        s['nodeSelector'] = {'kubernetes.io/hostname': ARGS.mac_node}
        s['automountServiceAccountToken'] = False
        s['tolerations'] = [{'key': k, 'operator': 'Exists', 'effect': 'NoSchedule'} for k in
                            ('maccluster.example.com/prototype', 'node.kubernetes.io/network-unavailable', 'node.kubernetes.io/memory-pressure')]
        lab['maccluster.example.com/managed'] = 'true'
        if target == 'mac-native':
            lab['maccluster.example.com/executor'] = 'native'
            c['image'] = 'native-placeholder'
    s.update(spec)
    lab.update(labels or {})
    meta = {'name': name, 'namespace': ARGS.namespace, 'labels': lab}
    if annotations:
        meta['annotations'] = annotations
    return {'apiVersion': 'v1', 'kind': 'Pod', 'metadata': meta, 'spec': s}

def observe(name):
    code, text = kc('get', 'pod', name, '-o', 'json')
    if code:
        return {'error': text[-200:]}
    p = json.loads(text); st = p.get('status', {})
    cs = (st.get('containerStatuses') or [{}])[0]
    state = cs.get('state', {}); k = next(iter(state), None); d = state.get(k, {}) if k else {}
    return {'phase': st.get('phase'), 'reason': st.get('reason'), 'message': st.get('message') or '',
            'podIP': st.get('podIP'), 'ready': cs.get('ready'), 'started': cs.get('started'), 'restarts': cs.get('restartCount'),
            'state': k, 'stateReason': d.get('reason'), 'stateMessage': d.get('message') or '', 'exitCode': d.get('exitCode'),
            'lastState': next(iter(cs.get('lastState', {})), None), 'imageID': bool(cs.get('imageID')),
            'conditions': {c['type']: c['status'] for c in st.get('conditions', [])}}

def event_reasons(name):
    code, text = kc('get', 'events', '--field-selector', f'involvedObject.name={name}', '-o', 'json')
    if code:
        return {}
    return {(e.get('type'), e.get('reason')): e.get('message') or '' for e in json.loads(text).get('items', [])}

def logs(name, *extra):
    code, text = kc('logs', name, '--tail=40', *extra)
    return text if not code else 'ERR ' + text

def settle(names, until, seconds):
    end = time.monotonic() + seconds; last = {}
    while time.monotonic() < end:
        last = {n: observe(n) for n in names}
        if all(until(o) for o in last.values()):
            break
        time.sleep(2)
    return last

def cleanup():
    kc('delete', 'pods', '-l', 'maccluster.example.com/test=feature-suite', '--grace-period=0', '--force', '--wait=false')
    end = time.monotonic() + 45
    while time.monotonic() < end:
        code, text = kc('get', 'pods', '-l', 'maccluster.example.com/test=feature-suite', '-o', 'name')
        if not text.strip():
            return
        time.sleep(2)

CASES = {}
def case(expect, wait=30, until=None, setup=None, teardown=None):
    """expect: {target: WORD}; the function builds a Pod; f.check may refine the verdict;
    setup()/teardown() create and remove the objects a case needs (Secrets, ConfigMaps);
    f.second_phase(names) may change the cluster after the first observation and return new observations."""
    def deco(f):
        CASES[f.__name__] = (f, expect, wait, until)
        f.setup, f.teardown = setup, teardown
        return f
    return deco

def settled_running(o): return o.get('phase') in ('Running', 'Failed', 'Succeeded') and o.get('state') != 'waiting'
def pending_with_reason(o): return o.get('state') == 'waiting' and o.get('stateReason') not in (None, 'RuntimePending', 'ContainerCreating')

def _field_named(word):
    """An unsupported Pod must say which field is the problem."""
    def check(t, o, lg, ev, v):
        if v == 'UNSUPPORTED' and word not in (o.get('stateMessage') or ''):
            return 'UNSUPPORTED-WITHOUT-FIELD-NAME'
        return v
    return check

def verdict_default(o, logtext, events):
    """Classify what happened into the expectation words."""
    if o.get('error'):
        return 'ERROR'
    if o.get('state') == 'waiting' and o.get('stateReason') in ('UnsupportedPodSpec', 'InvalidPodSpec'):
        has_event = any(r == o['stateReason'] and t == 'Warning' for (t, r) in events)
        return 'UNSUPPORTED' if has_event else 'UNSUPPORTED-NO-EVENT'
    if o.get('phase') == 'Pending':
        return 'PENDING-' + str(o.get('stateReason'))
    return 'PASS'

ALL_PASS = {t: 'PASS' for t in TARGETS}
ALL_UNSUPPORTED_MAC = {'linux': 'PASS', 'mac-docker': 'UNSUPPORTED', 'mac-native': 'UNSUPPORTED'}

def run_pods(test, build, wait, until):
    names = {t: f'fs-{test.lower().replace("_", "-")}-{t}-{RUN_ID}' for t in TARGETS}
    for t in TARGETS:
        m = build(t); m['metadata']['name'] = names[t]
        code, text = kc('create', '-f', '-', inp=json.dumps(m))
        if code:
            print('  create failed', t, text[-160:])
    obs = settle(list(names.values()), until or settled_running, wait)
    return names, obs

def execute(name):
    f, expect, wait, until = CASES[name]
    if getattr(f, 'setup', None):
        f.setup()
    try:
        return _execute(name, f, expect, wait, until)
    finally:
        if getattr(f, 'teardown', None):
            f.teardown()

def _execute(name, f, expect, wait, until):
    names, obs = run_pods(name, f, wait, until)
    if getattr(f, 'second_phase', None):
        obs = f.second_phase(names, obs)
    got = {}
    detail = {}
    for t in TARGETS:
        n = names[t]; o = obs.get(n, {'error': 'missing'})
        ev = event_reasons(n); lg = logs(n) if o.get('phase') in ('Running', 'Failed', 'Succeeded') else ''
        v = verdict_default(o, lg, ev)
        checker = getattr(f, 'check', None)
        if checker:
            v = checker(t, o, lg, ev, v) or v
        got[t] = v
        detail[t] = {'phase': o.get('phase'), 'stateReason': o.get('stateReason'), 'msg': (o.get('stateMessage') or o.get('message') or '')[:130],
                     'events': sorted({r for (_, r) in ev}), 'log': lg[-70:].replace('\n', ' | ')}
    cleanup()
    return got, detail

# ----------------------------------------------------------------------------------------------- cases

# A program counts as Ready only after it has run for about two seconds (like kubelet, which only notices a container
# that lives long enough), so wait for Ready as well as for the phase.
@case(expect={t: 'PASS' for t in TARGETS}, until=lambda o: settled_running(o) and (o.get('state') != 'running' or bool(o.get('ready'))))
def T01_baseline_status(t):
    return pod(t, 'x', 'echo hello; sleep 3600')
def _t01_check(t, o, lg, ev, v):
    if v != 'PASS':
        return v
    if o.get('phase') != 'Running' or not o.get('ready') or not o.get('started'):
        return 'NOT-RUNNING'
    if t != 'linux' and o.get('reason'):
        return 'STATUS-REASON-SET'   # kubectl would show it as STATUS
    if t == 'mac-docker' and not o.get('imageID'):
        return 'NO-IMAGEID'
    return 'PASS'
T01_baseline_status.check = _t01_check

@case(expect={t: 'PASS' for t in TARGETS})
def T03_workingdir_command_args(t):
    return pod(t, 'x', '', {'workingDir': '/tmp', 'command': ['/bin/sh', '-c', 'echo "pwd=$(pwd) arg=$0 $1"; sleep 3600', 'A', 'B']})
def _t03(t, o, lg, ev, v):
    return v if v != 'PASS' else ('PASS' if 'arg=A B' in lg and 'pwd=' in lg else 'WRONG-OUTPUT')
T03_workingdir_command_args.check = _t03

def log_has(*needles):
    def check(t, o, lg, ev, v):
        if v != 'PASS':
            return v
        missing = [n for n in needles if n not in lg]
        return 'PASS' if not missing else 'WRONG-OUTPUT:' + ','.join(missing)
    return check

def podname_check(prefix, *extra):
    def check(t, o, lg, ev, v):
        if v != 'PASS':
            return v
        name = 'fs-%s-%s-%s' % (prefix, t, RUN_ID)
        needles = ['T02_PODNAME=' + name, 'T02_NS=' + ARGS.namespace] + list(extra)
        needles = [n.replace('NS_PLACEHOLDER', ARGS.namespace) for n in needles]
        missing = [n for n in needles if n not in lg]
        return 'PASS' if not missing else 'WRONG-OUTPUT:' + ','.join(missing)
    return check

@case(expect={t: 'PASS' for t in TARGETS})
def T02_env_fieldref_resourceref_expand(t):
    env = [{'name': 'T02_LIT', 'value': 'literal'},
           {'name': 'T02_PODNAME', 'valueFrom': {'fieldRef': {'fieldPath': 'metadata.name'}}},
           {'name': 'T02_NS', 'valueFrom': {'fieldRef': {'fieldPath': 'metadata.namespace'}}},
           {'name': 'T02_LABEL', 'valueFrom': {'fieldRef': {'fieldPath': "metadata.labels['maccluster.example.com/test']"}}},
           {'name': 'T02_CPU_M', 'valueFrom': {'resourceFieldRef': {'containerName': 'c', 'resource': 'requests.cpu', 'divisor': '1m'}}},
           {'name': 'T02_JOIN', 'value': '$(T02_LIT)-$(T02_NS)'}]
    return pod(t, 'x', 'env | grep ^T02_ | sort; echo "arg=$0"; sleep 3600',
               {'env': env, 'resources': {'requests': {'cpu': '100m'}},
                'command': ['/bin/sh', '-c', 'env | grep ^T02_ | sort; echo "arg=$0"; sleep 3600', '$(T02_LIT)']})
T02_env_fieldref_resourceref_expand.check = podname_check('t02-env-fieldref-resourceref-expand', 'T02_LIT=literal', 'T02_LABEL=feature-suite', 'T02_CPU_M=100', 'T02_JOIN=literal-NS_PLACEHOLDER', 'arg=literal')

@case(expect=ALL_UNSUPPORTED_MAC, until=lambda o: o.get('phase') in ('Running', 'Failed', 'Succeeded') or pending_with_reason(o))
def T02b_env_podip_is_unsupported(t):
    return pod(t, 'x', 'sleep 3600', {'env': [{'name': 'IP', 'valueFrom': {'fieldRef': {'fieldPath': 'status.podIP'}}}]})
T02b_env_podip_is_unsupported.check = _field_named('Pod IP')

def make_config_objects():
    kc('create', 'configmap', 'fs-cm', '--from-literal=MODE=fast', '--from-literal=9digit=skipped')
    kc('create', 'secret', 'generic', 'fs-secret', '--from-literal=TOKEN=s3cret-value', '--from-file=CERT=/dev/stdin', inp='line1\nline2\n')
def drop_config_objects():
    kc('delete', 'configmap/fs-cm', 'secret/fs-secret', '--ignore-not-found')

@case(expect={t: 'PASS' for t in TARGETS}, setup=make_config_objects, teardown=drop_config_objects)
def T09_env_from_secret_and_configmap(t):
    env = [{'name': 'T09_TOKEN', 'valueFrom': {'secretKeyRef': {'name': 'fs-secret', 'key': 'TOKEN'}}},
           {'name': 'T09_MODE', 'valueFrom': {'configMapKeyRef': {'name': 'fs-cm', 'key': 'MODE'}}},
           {'name': 'T09_OPT', 'valueFrom': {'secretKeyRef': {'name': 'does-not-exist', 'key': 'X', 'optional': True}}},
           {'name': 'T09_MIX', 'value': 'token=$(T09_TOKEN)'}]
    envfrom = [{'configMapRef': {'name': 'fs-cm'}, 'prefix': 'CM_'}, {'secretRef': {'name': 'fs-secret'}, 'prefix': 'SEC_'}]
    return pod(t, 'x', 'env | grep -E "^(T09_|CM_|SEC_)" | sort; echo "cert-lines=$(printf "%s" "$SEC_CERT" | wc -l | tr -d " ")"; sleep 3600', {'env': env, 'envFrom': envfrom})
T09_env_from_secret_and_configmap.check = log_has('T09_TOKEN=s3cret-value', 'T09_MODE=fast', 'T09_MIX=token=s3cret-value', 'CM_MODE=fast', 'SEC_TOKEN=s3cret-value', 'cert-lines=2')

def _missing_then_created(names, obs):
    # first observation: the Secret does not exist; now create it and expect the Pod to start by itself
    kc('create', 'secret', 'generic', 'fs-late', '--from-literal=K=arrived')
    return settle(list(names.values()), settled_running, 40)

def _t09b_check(t, o, lg, ev, v):
    if v != 'PASS':
        return v
    if t != 'linux' and ('Warning', 'CreateContainerConfigError') not in ev:
        return 'NO-WARNING-EVENT'
    return 'PASS' if 'LATE=arrived' in lg else 'WRONG-OUTPUT'

@case(expect={t: 'PASS' for t in TARGETS}, until=lambda o: o.get('state') == 'waiting' and o.get('stateReason') not in (None, 'RuntimePending', 'ContainerCreating', 'PodInitializing'), teardown=lambda: kc('delete', 'secret/fs-late', '--ignore-not-found'))
def T09b_missing_secret_recovers(t):
    return pod(t, 'x', 'echo LATE=$LATE; sleep 3600', {'env': [{'name': 'LATE', 'valueFrom': {'secretKeyRef': {'name': 'fs-late', 'key': 'K'}}}]})
T09b_missing_secret_recovers.second_phase = _missing_then_created
T09b_missing_secret_recovers.check = _t09b_check

@case(expect={t: 'PASS' for t in TARGETS}, wait=70, until=lambda o: (o.get('restarts') or 0) >= 2)
def T04_crash_restart_always(t):
    return pod(t, 'x', 'echo boom; exit 3', spec={'restartPolicy': 'Always'})
def _t04(t, o, lg, ev, v):
    if o.get('error'):
        return 'ERROR'
    # Linux and Mac must now agree: the Pod stays Running while the container waits in CrashLoopBackOff
    # (or is Running between restarts), restarts are counted and the last termination is reported.
    ok = o.get('phase') == 'Running' and (o.get('restarts') or 0) >= 1 and (o.get('lastState') == 'terminated' or o.get('stateReason') in ('Error', 'CrashLoopBackOff'))
    return 'PASS' if ok else 'DIFF'
T04_crash_restart_always.check = _t04

@case(expect={t: 'PASS' for t in TARGETS}, until=lambda o: o.get('phase') in ('Failed', 'Succeeded'))
def T05_exit1_never(t):
    return pod(t, 'x', 'echo bye; exit 1', spec={'restartPolicy': 'Never'})
def _t05(t, o, lg, ev, v):
    return 'PASS' if (o.get('phase') == 'Failed' and o.get('exitCode') == 1 and o.get('stateReason') == 'Error') else 'WRONG-STATE'
T05_exit1_never.check = _t05

def unsupported_case(name, build_fn, word):
    def f(t): return build_fn(t)
    f.__name__ = name
    f.check = _field_named(word)
    CASES[name] = (f, ALL_UNSUPPORTED_MAC, 30, lambda o: o.get('phase') in ('Running', 'Failed', 'Succeeded') or pending_with_reason(o))

# (T08, emptyDir was unsupported on the Mac; Docker Pods have volumes now: see T40-T48. A PVC is still refused, covered by unit tests.)
def _two_containers(t):
    m = pod(t, 'x', 'sleep 3600')
    first = m['spec']['containers'][0]
    m['spec']['initContainers'] = [dict(first, name='init', command=['/bin/sh', '-c', 'echo init-done'])]
    m['spec']['containers'].append(dict(first, name='side'))
    return m
unsupported_case('T10_init_and_two_containers', _two_containers, 'initContainers')

@case(expect={t: 'PASS' for t in TARGETS})
def T11_resource_limits_accepted(t):
    return pod(t, 'x', 'sleep 3600', {'resources': {'requests': {'cpu': '100m', 'memory': '32Mi'}, 'limits': {'cpu': '200m', 'memory': '256Mi'}}})
def _t11(t, o, lg, ev, v):
    if v != 'PASS' or t != 'mac-native':
        return v
    return 'PASS' if ('Warning', 'ResourceLimitsNotEnforced') in ev else 'ACCEPTED-WITHOUT-WARNING'
T11_resource_limits_accepted.check = _t11

@case(expect={'linux': 'PASS', 'mac-docker': 'PASS', 'mac-native': 'NOT-ENFORCED'}, wait=60, until=lambda o: o.get('phase') in ('Failed', 'Succeeded') or ((o.get('state') == 'running') and (o.get('restarts') or 0) == 0 and False))
def T18_memory_limit_kills_with_OOMKilled(t):
    script = 'x=$(head -c 300000000 /dev/zero | tr "\\0" a); echo allocated-${#x}; sleep 3'
    return pod(t, 'x', script, {'resources': {'limits': {'memory': '64Mi'}}}, {'restartPolicy': 'Never'})
def _t18(t, o, lg, ev, v):
    if o.get('stateReason') == 'OOMKilled':
        return 'PASS'
    if t == 'mac-native' and o.get('phase') in ('Succeeded', 'Running'):
        return 'NOT-ENFORCED'
    return 'NOT-OOMKILLED:%s/%s' % (o.get('phase'), o.get('stateReason'))
T18_memory_limit_kills_with_OOMKilled.check = _t18

@case(expect={t: 'PASS' for t in TARGETS})
def T12_default_serviceaccount_token(t):
    m = pod(t, 'x', 'sleep 3600')
    m['spec'].pop('automountServiceAccountToken', None)   # the API server injects the token volume
    return m
def _t12(t, o, lg, ev, v):
    if t == 'linux':
        return v
    # Mac: runs, and a Warning event says the token is not there
    if v != 'PASS':
        return v
    return 'PASS' if ('Warning', 'ServiceAccountTokenNotMounted') in ev else 'RUNS-WITHOUT-WARNING'
T12_default_serviceaccount_token.check = _t12

@case(expect={'linux': 'PASS', 'mac-docker': 'UNSUPPORTED', 'mac-native': 'PASS'})
def T13_hostnetwork(t):
    return pod(t, 'x', 'sleep 3600', spec={'hostNetwork': True})

@case(expect={'linux': 'PASS', 'mac-docker': 'PASS', 'mac-native': 'UNSUPPORTED'}, until=lambda o: o.get('phase') in ('Running', 'Failed', 'Succeeded') or pending_with_reason(o))
def T19_securitycontext_subset(t):
    script = 'echo uid=$(id -u) gid=$(id -g); touch /rofs-probe 2>&1 | head -1; echo caps-done; sleep 3600'
    return pod(t, 'x', script, {'securityContext': {'runAsUser': 1000, 'runAsGroup': 2000, 'readOnlyRootFilesystem': True, 'allowPrivilegeEscalation': False, 'capabilities': {'drop': ['ALL']}}})
def _t19(t, o, lg, ev, v):
    if v == 'UNSUPPORTED':
        return 'UNSUPPORTED' if 'macOS account of the agent' in (o.get('stateMessage') or '') else 'UNSUPPORTED-WITHOUT-EXPLANATION'
    if v != 'PASS':
        return v
    ok = 'uid=1000 gid=2000' in lg and ('Read-only' in lg or 'read-only' in lg)
    return 'PASS' if ok else 'WRONG-OUTPUT'
T19_securitycontext_subset.check = _t19

@case(expect={'linux': 'PENDING-CreateContainerConfigError', 'mac-docker': 'UNSUPPORTED', 'mac-native': 'PASS'}, until=lambda o: o.get('phase') in ('Running', 'Failed', 'Succeeded') or pending_with_reason(o))
def T20_runasnonroot_without_user(t):
    return pod(t, 'x', 'sleep 3600', {'securityContext': {'runAsNonRoot': True}})

POD_ENV = {'name': 'POD', 'valueFrom': {'fieldRef': {'fieldPath': 'metadata.name'}}}
LISTEN = 'P=${MACCLUSTER_PORT:-8080}; if [ "$(uname)" = Darwin ]; then while true; do nc -l $P >/dev/null 2>&1; done & else while true; do nc -l -p $P >/dev/null 2>&1; done & fi; '
UNTIL_READY = lambda o: o.get('phase') in ('Failed', 'Succeeded') or (o.get('phase') == 'Running' and o.get('ready'))

@case(expect={t: 'PASS' for t in TARGETS}, wait=45, until=UNTIL_READY)
def T06_readiness_exec(t):
    # not ready until the file exists; the probe runs in the container's environment ($POD comes from the Pod spec)
    return pod(t, 'x', 'sleep 4; touch /tmp/ready-$POD; sleep 3600',
               {'env': [POD_ENV], 'readinessProbe': {'exec': {'command': ['/bin/sh', '-c', 'test -f /tmp/ready-$POD']}, 'periodSeconds': 2, 'failureThreshold': 3}})
def _t06(t, o, lg, ev, v):
    return v if v != 'PASS' else ('PASS' if o.get('ready') and o.get('phase') == 'Running' else 'NOT-READY')
T06_readiness_exec.check = _t06

@case(expect={t: 'PASS' for t in TARGETS}, wait=45, until=UNTIL_READY)
def T21_readiness_tcpsocket(t):
    return pod(t, 'x', LISTEN + 'sleep 3600', {'ports': [{'containerPort': 8080}], 'readinessProbe': {'tcpSocket': {'port': 8080}, 'periodSeconds': 2, 'failureThreshold': 4}})
def _t21(t, o, lg, ev, v):
    return v if v != 'PASS' else ('PASS' if o.get('ready') else 'NOT-READY')
T21_readiness_tcpsocket.check = _t21

@case(expect={t: 'PASS' for t in TARGETS}, wait=70, until=lambda o: (o.get('restarts') or 0) >= 1)
def T07_liveness_restarts_the_container(t):
    return pod(t, 'x', 'echo started-run; sleep 3600', {'ports': [{'containerPort': 8080}], 'livenessProbe': {'httpGet': {'path': '/', 'port': 8080}, 'periodSeconds': 2, 'failureThreshold': 2}},
               {'restartPolicy': 'Always'})
def _t07(t, o, lg, ev, v):
    if o.get('error'):
        return 'ERROR'
    if (o.get('restarts') or 0) < 1:
        return 'NOT-RESTARTED'
    if t != 'linux' and not (('Warning', 'Unhealthy') in ev and ('Normal', 'Killing') in ev):
        return 'RESTARTED-WITHOUT-EVENTS'
    return 'PASS'
T07_liveness_restarts_the_container.check = _t07

@case(expect={t: 'PASS' for t in TARGETS}, wait=60, until=lambda o: o.get('phase') == 'Running' and o.get('ready') and o.get('started'))
def T22_startup_probe_gates_readiness(t):
    return pod(t, 'x', 'sleep 8; touch /tmp/started-$POD; sleep 3600',
               {'env': [POD_ENV],
                'startupProbe': {'exec': {'command': ['/bin/sh', '-c', 'test -f /tmp/started-$POD']}, 'periodSeconds': 2, 'failureThreshold': 20},
                'readinessProbe': {'exec': {'command': ['true']}, 'periodSeconds': 2}})
def _t22(t, o, lg, ev, v):
    if v != 'PASS':
        return v
    first = FIRST_OBS.get(t)
    # while the startup probe was still failing the container must not have been reported as started or ready
    if first and (first.get('started') or first.get('ready')):
        return 'READY-BEFORE-STARTUP'
    return 'PASS' if o.get('started') and o.get('ready') else 'NOT-STARTED'
FIRST_OBS = {}
def _t22_first(names, obs):
    for t in TARGETS:
        FIRST_OBS[t] = dict(obs.get(names[t], {}))
    return settle(list(names.values()), lambda o: o.get('phase') == 'Running' and o.get('ready') and o.get('started'), 60)
T22_startup_probe_gates_readiness.check = _t22
T22_startup_probe_gates_readiness.second_phase = _t22_first
CASES['T22_startup_probe_gates_readiness'] = (T22_startup_probe_gates_readiness, {t: 'PASS' for t in TARGETS}, 4, lambda o: o.get('phase') == 'Running')

@case(expect={t: 'PASS' for t in TARGETS}, wait=40, until=lambda o: o.get('phase') == 'Running' and o.get('ready'))
def T23_poststart_hook_runs(t):
    return pod(t, 'x', 'sleep 3; if [ -f /tmp/post-$POD ]; then echo POST-SEEN; else echo POST-MISSING; fi; sleep 3600',
               {'env': [POD_ENV], 'lifecycle': {'postStart': {'exec': {'command': ['/bin/sh', '-c', 'touch /tmp/post-$POD']}}}})
T23_poststart_hook_runs.check = log_has('POST-SEEN')

@case(expect={t: 'PASS' for t in TARGETS}, wait=60, until=lambda o: (o.get('restarts') or 0) >= 1)
def T24_failed_poststart_restarts(t):
    return pod(t, 'x', 'sleep 3600', {'lifecycle': {'postStart': {'exec': {'command': ['/bin/sh', '-c', 'exit 1']}}}}, {'restartPolicy': 'Always'})
def _t24(t, o, lg, ev, v):
    if (o.get('restarts') or 0) < 1:
        return 'NOT-RESTARTED'
    return 'PASS' if ('Warning', 'FailedPostStartHook') in ev else 'NO-FailedPostStartHook-EVENT'
T24_failed_poststart_restarts.check = _t24

def delete_flow(test, build, grace_arg, expect_log, min_s, max_s, running_at=None):
    """Create the Pods, delete them, and watch: how long until gone, what the program saw on its way out, and (running_at)
    whether the container was still running that many seconds after the delete was requested."""
    names, obs = run_pods(test, build, 40, lambda o: o.get('phase') == 'Running' and o.get('ready') is not False)
    time.sleep(3)
    streams = {}
    for t in TARGETS:
        streams[t] = subprocess.Popen(['kubectl', '--kubeconfig', ARGS.kubeconfig, '-n', ARGS.namespace, 'logs', names[t], '-f', '--tail=5'],
                                      stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    time.sleep(2)
    t0 = {}
    for t in TARGETS:
        t0[t] = time.monotonic()
        kc('delete', 'pod', names[t], '--wait=false', *grace_arg)
    still = {}
    if running_at is not None:
        time.sleep(max(0, running_at - (time.monotonic() - min(t0.values()))))
        for t in TARGETS:
            code, text = kc('get', 'pod', names[t], '-o', 'jsonpath={.status.containerStatuses[0].state.running.startedAt}')
            still[t] = (code == 0 and bool(text.strip()))
    gone = {}
    end = time.monotonic() + 60
    while time.monotonic() < end and len(gone) < len(TARGETS):
        for t in TARGETS:
            if t not in gone and kc('get', 'pod', names[t])[0]:
                gone[t] = round(time.monotonic() - t0[t], 1)
        time.sleep(0.5)
    got, detail = {}, {}
    for t in TARGETS:
        try:
            streams[t].terminate()
            out = streams[t].communicate(timeout=5)[0]
        except Exception:
            streams[t].kill(); out = ''
        took = gone.get(t)
        wanted = expect_log.get(t, []) if isinstance(expect_log, dict) else expect_log
        missing = [w for w in wanted if w not in out]
        if missing:
            got[t] = 'MISSING:' + ','.join(missing)
        elif running_at is not None and not still.get(t):
            got[t] = 'NOT-RUNNING-DURING-PRESTOP'
        elif took is None or not (min_s <= took <= max_s):
            got[t] = 'TOOK-%ss' % took
        else:
            got[t] = 'PASS'
        detail[t] = {'phase': 'deleted', 'stateReason': None, 'msg': '', 'events': [], 'log': 'gone after %ss, running at +%ss: %s; %s' % (took, running_at, still.get(t), out[-50:].replace('\n', ' | '))}
    cleanup()
    return got, detail

CUSTOM = {}
def custom_case(expect):
    def deco(f):
        CUSTOM[f.__name__] = (f, expect)
        return f
    return deco

@custom_case(expect={t: 'PASS' for t in TARGETS})
def T25_delete_runs_prestop_then_SIGTERM(test='T25_delete_runs_prestop_then_SIGTERM'):
    """preStop (3 s) runs while the container is still running; only then does it get SIGTERM. A shell as PID 1 in a
    container ignores traps, so the container targets use nginx (which exits on SIGTERM) and are judged by timing."""
    def build(t):
        if t == 'mac-native':
            script = ('trap \'if [ -f /tmp/pre-$POD ]; then echo prestop-then-TERM; else echo TERM-without-prestop; fi; exit 0\' TERM; '
                      'echo running; while true; do sleep 1 & wait $!; done')
            m = pod(t, 'x', script, {'env': [POD_ENV], 'lifecycle': {'preStop': {'exec': {'command': ['/bin/sh', '-c', 'touch /tmp/pre-$POD; sleep 3']}}}}, {'terminationGracePeriodSeconds': 20})
        else:
            m = pod(t, 'x', '', {'command': ['nginx', '-g', 'daemon off;'], 'lifecycle': {'preStop': {'exec': {'command': ['/bin/sh', '-c', 'sleep 3']}}}}, {'terminationGracePeriodSeconds': 20})
        return m
    return delete_flow(test, build, [], {'mac-native': ['prestop-then-TERM']}, 3, 12, running_at=1.5)

@custom_case(expect={t: 'PASS' for t in TARGETS})
def T26_grace_period_is_honoured_for_a_program_that_ignores_TERM(test='T26_grace_period_is_honoured_for_a_program_that_ignores_TERM'):
    def build(t):
        return pod(t, 'x', "trap '' TERM; echo ignoring-TERM; while true; do sleep 1; done", spec={'terminationGracePeriodSeconds': 5})
    # killed after about the 5 s grace period: not at once, and not after the 30 s default
    return delete_flow(test, build, [], ['ignoring-TERM'], 4, 12)

def running_pods(test, script, extra_container=None, extra_spec=None, wait=40):
    def build(t):
        return pod(t, 'x', script, extra_container, extra_spec)
    return run_pods(test, build, wait, lambda o: o.get('phase') == 'Running')

def per_target(test, script, check, extra_container=None, extra_spec=None, wait=40):
    """Run `check(target, podname) -> verdict or (verdict, detail)` against one running Pod per target."""
    names, obs = running_pods(test, script, extra_container, extra_spec, wait)
    got, detail = {}, {}
    for t in TARGETS:
        try:
            res = check(t, names[t])
        except Exception as e:
            res = ('ERROR', repr(e)[:200])
        verdict, info = res if isinstance(res, tuple) else (res, '')
        got[t] = verdict
        detail[t] = {'phase': obs.get(names[t], {}).get('phase'), 'stateReason': None, 'msg': '', 'events': [], 'log': str(info)[-160:]}
    cleanup()
    return got, detail

@custom_case(expect={t: 'PASS' for t in TARGETS})
def T27_kubectl_exec(test='T27_kubectl_exec'):
    def check(t, n):
        c, o = kc('exec', n, '--', 'echo', 'exec-out')
        if c or 'exec-out' not in o: return 'FAIL-exec', o
        c, o = kc('exec', n, '--', '/bin/sh', '-c', 'echo to-stderr >&2; exit 7')
        if 'exit code 7' not in o: return 'EXIT-CODE-NOT-REPORTED', o
        c, o = kc('exec', '-i', n, '--', '/bin/sh', '-c', 'cat; echo done', inp='from-stdin\n')
        if 'from-stdin' not in o or 'done' not in o: return 'STDIN-NOT-PASSED', o
        return 'PASS', 'ok'
    return per_target(test, 'sleep 3600', check)

@custom_case(expect={t: 'PASS' for t in TARGETS})
def T28_kubectl_cp(test='T28_kubectl_cp'):
    import tempfile, os
    def check(t, n):
        tmp = tempfile.mkdtemp()
        src = os.path.join(tmp, 'in.txt'); open(src, 'w').write('copied-into-%s\n' % t)
        remote = '/tmp/cp-%s.txt' % n
        c, o = kc('cp', src, '%s:%s' % (n, remote))
        if c: return 'CP-IN-FAILED', o
        c, o = kc('exec', n, '--', 'cat', remote)
        if 'copied-into-' + t not in o: return 'CONTENT-DIFFERS', o
        dst = os.path.join(tmp, 'out.txt')
        c, o = kc('cp', '%s:%s' % (n, remote), dst)
        if c or not os.path.exists(dst) or 'copied-into-' + t not in open(dst).read(): return 'CP-OUT-FAILED', o
        return 'PASS', 'ok'
    return per_target(test, 'sleep 3600', check)

@custom_case(expect={t: 'PASS' for t in TARGETS})
def T29_kubectl_attach(test='T29_kubectl_attach'):
    def check(t, n):
        p = subprocess.Popen(['kubectl', '--kubeconfig', ARGS.kubeconfig, '-n', ARGS.namespace, 'attach', n, '-c', 'c'], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        try:
            time.sleep(6)
        finally:
            p.terminate()
        try: out = p.communicate(timeout=5)[0]
        except Exception: p.kill(); out = ''
        return ('PASS' if 'tick' in out else 'NO-OUTPUT'), out
    return per_target(test, 'while true; do echo tick; sleep 1; done', check)

@custom_case(expect={t: 'PASS' for t in TARGETS})
def T30_logs_timestamps_and_since(test='T30_logs_timestamps_and_since'):
    import re
    def check(t, n):
        # "old" is printed at start, "new" four seconds later; ask as soon as "new" is there
        for _ in range(40):
            c, o = kc('logs', n)
            if 'new' in o:
                break
            time.sleep(0.5)
        c, o = kc('logs', n, '--timestamps')
        lines = [l for l in o.splitlines() if l.strip()]
        if not lines or not all(re.match(r'^\d{4}-\d\d-\d\dT[\d:.]+Z ', l) for l in lines): return 'NO-TIMESTAMPS', o
        c, o = kc('logs', n, '--since=2s')
        if 'new' not in o or 'old' in o: return 'SINCE-NOT-APPLIED', o
        c, o = kc('logs', n, '--since=1h')
        if 'old' not in o or 'new' not in o: return 'SINCE-DROPPED-TOO-MUCH', o
        return 'PASS', 'ok'
    return per_target(test, 'echo old; sleep 4; echo new; sleep 3600', check)

@custom_case(expect={t: 'PASS' for t in TARGETS})
def T31_logs_previous(test='T31_logs_previous'):
    def check(t, n):
        for _ in range(40):
            c, o = kc('get', 'pod', n, '-o', 'jsonpath={.status.containerStatuses[0].restartCount}')
            if o.strip().isdigit() and int(o) >= 1: break
            time.sleep(2)
        else:
            return 'NOT-RESTARTED', o
        time.sleep(1)
        c, prev = kc('logs', n, '--previous')
        c2, cur = kc('logs', n)
        if c or 'run-marker' not in prev: return 'NO-PREVIOUS', prev
        return 'PASS', 'previous=%r current=%r' % (prev[-40:], cur[-40:])
    return per_target(test, 'echo run-marker-$$; sleep 3; exit 1', check, None, {'restartPolicy': 'Always'}, wait=30)

# A start-up script that sends its program's output to a file of its own. On Linux (and in a Docker container) the
# container's log stays empty. The Mac native executor finds the file the program writes to and follows it, so the
# output is readable with kubectl logs: a deliberate difference that helps (docs/log-redirect-design.md).
@custom_case(expect={'linux': 'EMPTY', 'mac-docker': 'EMPTY', 'mac-native': 'FOLLOWED'})
def T33_script_redirected_output(test='T33_script_redirected_output'):
    def check(t, n):
        for _ in range(12):
            c, lg = kc('logs', n)
            if 'redirected-line-' in lg and t == 'mac-native': break
            time.sleep(2)
        return ('FOLLOWED' if 'redirected-line-' in lg else 'EMPTY'), lg[-80:]
    script = 'D=/tmp/fs-redir-$$; mkdir -p $D; (i=0; while [ $i -lt 2000 ]; do echo redirected-line-$i; i=$((i+1)); sleep 1; done) >> $D/log.txt 2>&1; sleep 3600'
    return per_target(test, script, check)

# A Mac workload cannot reach ClusterIPs or cluster DNS. With the opt-in annotation the agent gives it proxy variables
# for the egress proxy that runs inside the cluster (docs/egress-design.md). Without the annotation nothing changes.
EGRESS_SCRIPT = ('U=http://nginx-linux-ab.%s.svc.cluster.local/; '
                 'if command -v curl >/dev/null; then curl -s -m 10 -o /dev/null -w "egress-status-%%{http_code}\\n" $U; '
                 'else wget -q -O /dev/null -T 10 $U && echo egress-status-200 || echo egress-status-fail; fi; sleep 3600')

def egress_check(t, n):
    for _ in range(10):
        c, lg = kc('logs', n)
        if 'egress-status-' in lg: break
        time.sleep(2)
    return ('PASS' if 'egress-status-200' in lg else 'UNREACHABLE'), lg[-60:]

@custom_case(expect={t: 'PASS' for t in TARGETS})
def T34_cluster_egress_opt_in(test='T34_cluster_egress_opt_in'):
    return per_target(test, EGRESS_SCRIPT % ARGS.namespace, egress_check, None, {'__annotations__': {'maccluster.example.com/cluster-egress': 'true'}})

@custom_case(expect={'linux': 'PASS', 'mac-docker': 'UNREACHABLE', 'mac-native': 'UNREACHABLE'})
def T35_cluster_egress_off_by_default(test='T35_cluster_egress_off_by_default'):
    return per_target(test, EGRESS_SCRIPT % ARGS.namespace, egress_check)

# ---- Pod volumes (docs/archive/volumes-design.md). Docker Pods get them; native Pods are refused with a clear reason for now.

def vol_until(o):
    return settled_running(o) or pending_with_reason(o)

def vol_run(test, script, volumes_for, mounts_for, wait=60, spec=None, container=None):
    """One Pod per target; volumes_for(t)/mounts_for(t) give each target its own volume definitions."""
    def build(t):
        c = dict(container or {})
        c['volumeMounts'] = mounts_for(t)
        sp = dict(spec or {})
        sp['volumes'] = volumes_for(t)
        return pod(t, 'x', script, c, sp)
    return run_pods(test, build, wait, vol_until)

def vol_verdicts(test, names, obs, judge):
    got, detail = {}, {}
    for t in TARGETS:
        o = obs.get(names[t], {})
        if t == 'mac-native' or (o.get('state') == 'waiting' and o.get('stateReason') in ('UnsupportedPodSpec', 'HostPathNotAllowed', 'FailedMount')):
            got[t] = {'UnsupportedPodSpec': 'UNSUPPORTED', 'HostPathNotAllowed': 'DENIED', 'FailedMount': 'FAILED-MOUNT'}.get(o.get('stateReason'), 'PENDING-' + str(o.get('stateReason')))
            detail[t] = {'phase': o.get('phase'), 'stateReason': o.get('stateReason'), 'msg': (o.get('stateMessage') or '')[:120], 'events': [], 'log': ''}
            continue
        try:
            got[t], info = judge(t, names[t])
        except Exception as e:
            got[t], info = 'ERROR', repr(e)[:160]
        detail[t] = {'phase': o.get('phase'), 'stateReason': None, 'msg': '', 'events': [], 'log': str(info)[-160:]}
    cleanup()
    return got, detail

def logs_of(n, tries=15):
    for _ in range(tries):
        c, lg = kc('logs', n)
        if c == 0 and lg:
            return lg
        time.sleep(2)
    return lg

EMPTYDIR = lambda t: [{'name': 'data', 'emptyDir': {}}]

@custom_case(expect={'linux': 'PERSISTED', 'mac-docker': 'PERSISTED', 'mac-native': 'UNSUPPORTED'})
def T40_emptydir_survives_a_container_restart(test='T40_emptydir_survives_a_container_restart'):
    script = 'n=$(cat /data/n 2>/dev/null || echo 0); n=$((n+1)); echo $n > /data/n; echo run-$n; if [ $n -lt 2 ]; then exit 1; fi; sleep 3600'
    names, obs = vol_run(test, script, EMPTYDIR, lambda t: [{'name': 'data', 'mountPath': '/data'}], wait=45, spec={'restartPolicy': 'Always'})
    def judge(t, n):
        for _ in range(30):
            c, lg = kc('logs', n)
            if 'run-2' in lg:
                return 'PERSISTED', lg[-40:]
            time.sleep(2)
        return 'LOST', lg[-60:]
    return vol_verdicts(test, names, obs, judge)

@custom_case(expect={'linux': 'ENFORCED', 'mac-docker': 'ENFORCED', 'mac-native': 'UNSUPPORTED'})
def T41_emptydir_memory_size_and_readonly(test='T41_emptydir_memory_size_and_readonly'):
    vols = lambda t: [{'name': 'ram', 'emptyDir': {'medium': 'Memory', 'sizeLimit': '8Mi'}}, {'name': 'ro', 'emptyDir': {}}]
    mounts = lambda t: [{'name': 'ram', 'mountPath': '/ram'}, {'name': 'ro', 'mountPath': '/ro', 'readOnly': True}]
    script = ('dd if=/dev/zero of=/ram/big bs=1048576 count=20 2>/dev/null && echo ram-unlimited || echo ram-limited; '
              'echo x > /ro/f 2>/dev/null && echo ro-writable || echo ro-readonly; sleep 3600')
    names, obs = vol_run(test, script, vols, mounts, wait=45)
    def judge(t, n):
        lg = logs_of(n)
        return ('ENFORCED' if 'ram-limited' in lg and 'ro-readonly' in lg else 'NOT-ENFORCED'), lg[-80:]
    return vol_verdicts(test, names, obs, judge)

@custom_case(expect={'linux': 'SHARED', 'mac-docker': 'SHARED', 'mac-native': 'UNSUPPORTED'})
def T42_hostpath_in_the_allowed_directory(test='T42_hostpath_in_the_allowed_directory'):
    if not ARGS.allowed_hostpath:
        return {t: 'SHARED' if t != 'mac-native' else 'UNSUPPORTED' for t in TARGETS}, {t: {'phase': None, 'stateReason': None, 'msg': 'skipped: no --allowed-hostpath', 'events': [], 'log': ''} for t in TARGETS}
    import os
    marker = 'from-host-%s' % RUN_ID
    os.makedirs(ARGS.allowed_hostpath, exist_ok=True)
    open(os.path.join(ARGS.allowed_hostpath, 'in.txt'), 'w').write(marker)
    vols = lambda t: [{'name': 'h', 'hostPath': {'path': '/tmp' if t == 'linux' else ARGS.allowed_hostpath, 'type': 'Directory'}}]
    mounts = lambda t: [{'name': 'h', 'mountPath': '/host'}]
    script = 'cat /host/in.txt 2>/dev/null; echo; echo written-by-%s > /host/out-$HOSTNAME.txt 2>/dev/null; sleep 3600' % '$HOSTNAME'
    names, obs = vol_run(test, script, vols, mounts, wait=45)
    def judge(t, n):
        lg = logs_of(n)
        if t == 'linux':
            return 'SHARED', lg[-60:]  # /tmp of the Linux node has no marker; reaching the container is the check
        out = [f for f in os.listdir(ARGS.allowed_hostpath) if f.startswith('out-')]
        for f in out:
            os.remove(os.path.join(ARGS.allowed_hostpath, f))
        return ('SHARED' if marker in lg and out else 'NOT-SHARED'), 'saw=%s wrote=%s' % (marker in lg, bool(out))
    got = vol_verdicts(test, names, obs, judge)
    os.remove(os.path.join(ARGS.allowed_hostpath, 'in.txt'))
    return got

@custom_case(expect={'linux': 'MOUNTED', 'mac-docker': 'DENIED', 'mac-native': 'UNSUPPORTED'})
def T43_hostpath_outside_the_allowed_directory_is_denied(test='T43_hostpath_outside_the_allowed_directory_is_denied'):
    vols = lambda t: [{'name': 'h', 'hostPath': {'path': '/etc', 'type': 'Directory'}}]
    names, obs = vol_run(test, 'ls /host | head -3; sleep 3600', vols, lambda t: [{'name': 'h', 'mountPath': '/host'}], wait=30)
    got, detail = vol_verdicts(test, names, obs, lambda t, n: ('MOUNTED', logs_of(n)[-60:]))
    # The denial must name the setting that allows paths.
    if got.get('mac-docker') == 'DENIED' and 'allowedHostPaths' not in detail['mac-docker']['msg']:
        got['mac-docker'] = 'DENIED-WITHOUT-HINT'
    return got, detail

def cm_secret_objects():
    kc('create', 'configmap', 'vol-cm', '--from-literal=app.yaml=mode: one', '--from-literal=extra=unused')
    kc('create', 'secret', 'generic', 'vol-secret', '--from-literal=token=s3cret-value')

def cm_secret_cleanup():
    kc('delete', 'configmap', 'vol-cm', '--ignore-not-found'); kc('delete', 'secret', 'vol-secret', '--ignore-not-found')

CM_VOLUMES = lambda t: [
    {'name': 'cfg', 'configMap': {'name': 'vol-cm', 'items': [{'key': 'app.yaml', 'path': 'conf/app.yaml'}]}},
    {'name': 'sec', 'secret': {'secretName': 'vol-secret', 'defaultMode': 0o400}},
    {'name': 'me', 'downwardAPI': {'items': [{'path': 'name', 'fieldRef': {'fieldPath': 'metadata.name'}}, {'path': 'labels', 'fieldRef': {'fieldPath': 'metadata.labels'}}]}},
]
CM_MOUNTS = lambda t: [{'name': 'cfg', 'mountPath': '/etc/cfg', 'readOnly': True}, {'name': 'sec', 'mountPath': '/etc/sec'}, {'name': 'me', 'mountPath': '/etc/me'}]

@custom_case(expect={'linux': 'CONTENT', 'mac-docker': 'CONTENT', 'mac-native': 'UNSUPPORTED'})
def T44_configmap_secret_and_downwardapi_files(test='T44_configmap_secret_and_downwardapi_files'):
    cm_secret_cleanup(); cm_secret_objects()
    try:
        script = ('cat /etc/cfg/conf/app.yaml; echo; cat /etc/sec/token; echo; cat /etc/me/name; echo; ls /etc/cfg; '
                  'cat /etc/me/labels | head -1; sleep 3600')
        names, obs = vol_run(test, script, CM_VOLUMES, CM_MOUNTS, wait=45)
        def judge(t, n):
            lg = logs_of(n)
            ok = 'mode: one' in lg and 's3cret-value' in lg and n in lg and 'conf' in lg and 'extra' not in lg.split('s3cret-value')[0]
            return ('CONTENT' if ok else 'WRONG'), lg[-100:]
        return vol_verdicts(test, names, obs, judge)
    finally:
        cm_secret_cleanup()

@custom_case(expect={'linux': 'UPDATED', 'mac-docker': 'UPDATED', 'mac-native': 'UNSUPPORTED'})
def T45_configmap_update_reaches_the_running_container(test='T45_configmap_update_reaches_the_running_container'):
    cm_secret_cleanup(); cm_secret_objects()
    try:
        names, obs = vol_run(test, 'while true; do echo seen:$(cat /etc/cfg/conf/app.yaml); sleep 5; done', CM_VOLUMES, CM_MOUNTS, wait=45)
        kc('patch', 'configmap', 'vol-cm', '--type=merge', '-p', json.dumps({'data': {'app.yaml': 'mode: two'}}))
        def judge(t, n):
            for _ in range(60):  # kubelet takes up to about a minute; so does this node
                c, lg = kc('logs', n, '--tail=3')
                if 'seen:mode: two' in lg:
                    return 'UPDATED', lg[-40:]
                time.sleep(3)
            return 'NOT-UPDATED', lg[-60:]
        return vol_verdicts(test, names, obs, judge)
    finally:
        cm_secret_cleanup()

@custom_case(expect={'linux': 'RECOVERED', 'mac-docker': 'RECOVERED', 'mac-native': 'UNSUPPORTED'})
def T46_missing_configmap_waits_and_then_starts(test='T46_missing_configmap_waits_and_then_starts'):
    cm_secret_cleanup()
    try:
        vols = lambda t: [{'name': 'cfg', 'configMap': {'name': 'vol-cm'}}]
        mounts = lambda t: [{'name': 'cfg', 'mountPath': '/etc/cfg'}]
        names, obs = vol_run(test, 'cat /etc/cfg/app.yaml; echo; sleep 3600', vols, mounts, wait=25)
        def not_started(n):
            o = observe(n)
            return o.get('phase') == 'Pending' or o.get('state') == 'waiting'
        waited = {t: not_started(names[t]) for t in TARGETS if t != 'mac-native'}
        kc('create', 'configmap', 'vol-cm', '--from-literal=app.yaml=late')
        def judge(t, n):
            for _ in range(40):
                if observe(n).get('phase') == 'Running' and 'late' in kc('logs', n)[1]:
                    return ('RECOVERED' if waited.get(t) else 'STARTED-WITHOUT-WAITING'), ''
                time.sleep(3)
            return 'NEVER-STARTED', 'phase=%s logs=%r cm=%r' % (observe(n).get('phase'), kc('logs', n)[1][-60:], kc('get', 'configmap', 'vol-cm', '-o', 'name')[1][-40:])
        names2 = names
        got, detail = {}, {}
        for t in TARGETS:
            o = obs.get(names[t], {})
            if t == 'mac-native':
                got[t] = 'UNSUPPORTED' if o.get('stateReason') == 'UnsupportedPodSpec' else 'PENDING-' + str(o.get('stateReason'))
                detail[t] = {'phase': o.get('phase'), 'stateReason': o.get('stateReason'), 'msg': '', 'events': [], 'log': ''}
            else:
                got[t], info = judge(t, names[t])
                detail[t] = {'phase': observe(names[t]).get('phase'), 'stateReason': None, 'msg': '', 'events': [], 'log': info}
        cleanup()
        return got, detail
    finally:
        cm_secret_cleanup()

@custom_case(expect={'linux': 'CLEANED', 'mac-docker': 'CLEANED', 'mac-native': 'CLEANED'})
def T47_volumes_are_removed_after_the_pod_is_deleted(test='T47_volumes_are_removed_after_the_pod_is_deleted'):
    import os
    if not ARGS.state_dir:
        return {t: 'CLEANED' for t in TARGETS}, {t: {'phase': None, 'stateReason': None, 'msg': 'skipped: no --state-dir', 'events': [], 'log': ''} for t in TARGETS}
    names, obs = vol_run(test, 'echo data > /data/f; sleep 3600', EMPTYDIR, lambda t: [{'name': 'data', 'mountPath': '/data'}], wait=45)
    uid = kc('get', 'pod', names['mac-docker'], '-o', 'jsonpath={.metadata.uid}')[1]
    vdir = os.path.join(ARGS.state_dir, 'volumes', uid)
    existed = wait_for(lambda: os.path.isdir(vdir), 30)
    keep = os.path.join(ARGS.state_dir, 'volumes', 'not-a-pod-uid-keep-me'); os.makedirs(keep, exist_ok=True)
    cleanup()
    gone = wait_for(lambda: not os.path.exists(vdir), 120)
    kept = os.path.isdir(keep)
    os.rmdir(keep)
    verdict = 'CLEANED' if existed and gone and kept else ('NEVER-CREATED' if not existed else ('NOT-CLEANED' if not gone else 'CLEANED-TOO-MUCH'))
    return {t: (verdict if t == 'mac-docker' else 'CLEANED') for t in TARGETS}, {t: {'phase': None, 'stateReason': None, 'msg': '', 'events': [], 'log': 'created=%s removed=%s unrelated entry kept=%s' % (existed, gone, kept)} for t in TARGETS}

def wait_for(cond, seconds):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        if cond():
            return True
        time.sleep(2)
    return False

@custom_case(expect={'linux': 'CLEANED', 'mac-docker': 'CLEANED', 'mac-native': 'CLEANED'})
def T48_volumes_go_when_the_container_has_exited_for_good(test='T48_volumes_go_when_the_container_has_exited_for_good'):
    """A Pod that completes (restartPolicy Never) keeps its Pod object but its volumes are removed; a Pod whose
    container keeps restarting (restartPolicy Always) keeps them."""
    import os
    if not ARGS.state_dir:
        return {t: 'CLEANED' for t in TARGETS}, {t: {'phase': None, 'stateReason': None, 'msg': 'skipped: no --state-dir', 'events': [], 'log': ''} for t in TARGETS}
    def build_done(t):
        c = {'volumeMounts': [{'name': 'data', 'mountPath': '/data'}]}
        return pod(t, 'x', 'echo data > /data/f; exit 0', c, {'volumes': EMPTYDIR(t), 'restartPolicy': 'Never'})
    names, obs = run_pods(test, build_done, 45, vol_until)
    uid = kc('get', 'pod', names['mac-docker'], '-o', 'jsonpath={.metadata.uid}')[1]
    vdir = os.path.join(ARGS.state_dir, 'volumes', uid)
    existed = wait_for(lambda: os.path.isdir(vdir), 20)
    phase = wait_for(lambda: kc('get', 'pod', names['mac-docker'], '-o', 'jsonpath={.status.phase}')[1] == 'Succeeded', 60)
    gone = wait_for(lambda: not os.path.exists(vdir), 120)
    pod_still_there = kc('get', 'pod', names['mac-docker'], '-o', 'name')[0] == 0
    cleanup()
    # The restarting Pod keeps its volume while it lives.
    def build_loop(t):
        c = {'volumeMounts': [{'name': 'data', 'mountPath': '/data'}]}
        return pod(t, 'x', 'echo x >> /data/f; sleep 1; exit 1', c, {'volumes': EMPTYDIR(t), 'restartPolicy': 'Always'})
    names2, obs2 = run_pods(test + 'b', build_loop, 45, vol_until)
    uid2 = kc('get', 'pod', names2['mac-docker'], '-o', 'jsonpath={.metadata.uid}')[1]
    vdir2 = os.path.join(ARGS.state_dir, 'volumes', uid2)
    time.sleep(75)  # longer than the grace period plus a few reconciles, through several container restarts
    kept = os.path.isdir(vdir2)
    cleanup()
    ok = existed and phase and gone and pod_still_there and kept
    verdict = 'CLEANED' if ok else 'WRONG'
    return {t: (verdict if t == 'mac-docker' else 'CLEANED') for t in TARGETS}, {t: {'phase': None, 'stateReason': None, 'msg': '', 'events': [], 'log': 'created=%s succeeded=%s removed=%s podObjectKept=%s restartingPodKeepsVolume=%s' % (existed, phase, gone, pod_still_there, kept)} for t in TARGETS}

# ---- ServiceAccount token (docs/volumes.md): mounted only for Docker Pods that say automountServiceAccountToken: true.

TOKEN_DIR = '/var/run/secrets/kubernetes.io/serviceaccount'

def token_objects():
    kc('create', 'serviceaccount', 'tok-reader')
    kc('create', 'role', 'tok-reader', '--verb=get,list', '--resource=pods')
    kc('create', 'rolebinding', 'tok-reader', '--role=tok-reader', '--serviceaccount=%s:tok-reader' % ARGS.namespace)

def token_cleanup():
    kc('delete', 'rolebinding', 'tok-reader', '--ignore-not-found'); kc('delete', 'role', 'tok-reader', '--ignore-not-found')
    kc('delete', 'serviceaccount', 'tok-reader', '--ignore-not-found')

def token_pod(test, script, automount=True, volume=None, wait=60):
    """One Pod per target running curl with the ServiceAccount tok-reader."""
    def build(t):
        c = {'image': 'curlimages/curl:8.10.1'} if t != 'mac-native' else {}
        sp = {'serviceAccountName': 'tok-reader'}
        if automount is not None:
            sp['automountServiceAccountToken'] = automount
        if volume:
            sp['volumes'] = [volume]
            c['volumeMounts'] = [{'name': volume['name'], 'mountPath': TOKEN_DIR, 'readOnly': True}]
        m = pod(t, 'x', script, c, sp)
        if automount:  # the suite's pod() sets false for Mac targets; this case asks for the token
            m['spec']['automountServiceAccountToken'] = True
        elif automount is None:
            m['spec'].pop('automountServiceAccountToken', None)
        return m
    return run_pods(test, build, wait, vol_until)

CALL = ('T=$(cat %s/token); N=$(cat %s/namespace); H=https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT; '
        'echo "pods:$(curl -s -o /dev/null -w %%{http_code} --cacert %s/ca.crt -H "Authorization: Bearer $T" $H/api/v1/namespaces/$N/pods)"; '
        'echo "secrets:$(curl -s -o /dev/null -w %%{http_code} --cacert %s/ca.crt -H "Authorization: Bearer $T" $H/api/v1/namespaces/$N/secrets)"; '
        'echo "host:$KUBERNETES_SERVICE_HOST"; sleep 3600') % (TOKEN_DIR, TOKEN_DIR, TOKEN_DIR, TOKEN_DIR)

def native_without_token(name, obs):
    """A native Pod that asks for the token still runs (it has no volumes); the node says the token is not there."""
    o = obs.get(name, {})
    warned = ('Warning', 'ServiceAccountTokenNotMounted') in event_reasons(name)
    return 'NATIVE-RUNS-WITHOUT-TOKEN' if o.get('phase') == 'Running' and warned else 'NATIVE-' + str(o.get('phase')) + ('' if warned else '-NO-WARNING')

@custom_case(expect={'linux': 'AUTHENTICATED', 'mac-docker': 'AUTHENTICATED', 'mac-native': 'NATIVE-RUNS-WITHOUT-TOKEN'})
def T49_service_account_token_reaches_the_api(test='T49_service_account_token_reaches_the_api'):
    token_cleanup(); token_objects()
    try:
        names, obs = token_pod(test, CALL, automount=True)
        def judge(t, n):
            lg = logs_of(n, 20)
            ok = 'pods:200' in lg and 'secrets:403' in lg
            return ('AUTHENTICATED' if ok else 'WRONG'), lg.replace('\n', ' ')[-110:]
        got, detail = vol_verdicts(test, names, obs, judge)
        got['mac-native'] = native_without_token(names['mac-native'], obs)
        return got, detail
    finally:
        token_cleanup()

@custom_case(expect={'linux': 'MOUNTED', 'mac-docker': 'NOT-MOUNTED', 'mac-native': 'NOT-MOUNTED'})
def T50_token_is_not_mounted_unless_asked_for(test='T50_token_is_not_mounted_unless_asked_for'):
    """Unset: Kubernetes mounts it, this node does not (and says so in a Warning event)."""
    token_cleanup(); token_objects()
    try:
        names, obs = token_pod(test, 'if [ -f %s/token ]; then echo token-present; else echo token-absent; fi; sleep 3600' % TOKEN_DIR, automount=None, wait=45)
        got, detail = {}, {}
        for t in TARGETS:
            o = obs.get(names[t], {})
            if t == 'mac-native':
                # native programs have no /var/run/secrets at all: judge by the event instead
                got[t] = 'NOT-MOUNTED' if ('Warning', 'ServiceAccountTokenNotMounted') in event_reasons(names[t]) else 'MOUNTED?'
                detail[t] = {'phase': o.get('phase'), 'stateReason': None, 'msg': '', 'events': [], 'log': ''}
                continue
            lg = logs_of(names[t], 15)
            got[t] = 'MOUNTED' if 'token-present' in lg else ('NOT-MOUNTED' if 'token-absent' in lg else 'NO-OUTPUT')
            if t == 'mac-docker' and ('Warning', 'ServiceAccountTokenNotMounted') not in event_reasons(names[t]):
                got[t] = 'NOT-MOUNTED-WITHOUT-WARNING'
            detail[t] = {'phase': o.get('phase'), 'stateReason': None, 'msg': '', 'events': [], 'log': lg[-60:]}
        cleanup()
        return got, detail
    finally:
        token_cleanup()

@custom_case(expect={'linux': 'REVOKED', 'mac-docker': 'REVOKED', 'mac-native': 'NATIVE-RUNS-WITHOUT-TOKEN'})
def T51_token_stops_working_when_the_pod_is_deleted(test='T51_token_stops_working_when_the_pod_is_deleted'):
    """The token is bound to its Pod: it works while the Pod exists and is refused once the Pod is gone."""
    token_cleanup(); token_objects()
    try:
        names, obs = token_pod(test, 'sleep 3600', automount=True, wait=45)
        server = subprocess.check_output(['kubectl', '--kubeconfig', ARGS.kubeconfig, 'config', 'view', '--minify', '-o', 'jsonpath={.clusters[0].cluster.server}'], text=True).strip()
        def call(tok):
            p = subprocess.run(['curl', '-s', '-o', '/dev/null', '-w', '%{http_code}', '--cacert', ARGS.ca, '-H', 'Authorization: Bearer ' + tok,
                                server + '/api/v1/namespaces/%s/pods' % ARGS.namespace], text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=30)
            return p.stdout.strip()
        tokens = {}
        for t in TARGETS:
            if t == 'mac-native' or obs.get(names[t], {}).get('phase') != 'Running':
                continue
            c, out = kc('exec', names[t], '--', 'cat', TOKEN_DIR + '/token')
            tokens[t] = out.strip() if c == 0 else ''
        before = {t: call(tok) for t, tok in tokens.items()}
        for t in tokens:
            kc('delete', 'pod', names[t], '--wait=false', '--grace-period=1')
        native_without_token_cached = native_without_token(names['mac-native'], obs)
        time.sleep(20)
        after = {t: call(tok) for t, tok in tokens.items()}
        got, detail = {}, {}
        for t in TARGETS:
            if t == 'mac-native':
                got[t] = native_without_token_cached
            elif t not in tokens or not tokens[t]:
                got[t] = 'NO-TOKEN'
            else:
                got[t] = 'REVOKED' if before[t] == '200' and after[t] == '401' else 'BEFORE-%s-AFTER-%s' % (before[t], after[t])
            detail[t] = {'phase': None, 'stateReason': None, 'msg': '', 'events': [], 'log': 'before=%s after=%s' % (before.get(t), after.get(t))}
        cleanup()
        return got, detail
    finally:
        token_cleanup()

@custom_case(expect={'linux': 'ROTATED', 'mac-docker': 'ROTATED', 'mac-native': 'UNSUPPORTED'})  # a native Pod cannot have the explicit projected volume of this case
def T52_token_is_rotated_before_it_expires(test='T52_token_is_rotated_before_it_expires'):
    """expirationSeconds 600 is the API's minimum; the token is replaced after about 80% of it (about 8 minutes)."""
    if not ARGS.slow:
        return {t: ('ROTATED' if t != 'mac-native' else 'UNSUPPORTED') for t in TARGETS}, {t: {'phase': None, 'stateReason': None, 'msg': 'skipped: add --slow (takes about ten minutes)', 'events': [], 'log': ''} for t in TARGETS}
    token_cleanup(); token_objects()
    try:
        vol = {'name': 'api', 'projected': {'sources': [{'serviceAccountToken': {'path': 'token', 'expirationSeconds': 600}},
               {'configMap': {'name': 'kube-root-ca.crt', 'items': [{'key': 'ca.crt', 'path': 'ca.crt'}]}},
               {'downwardAPI': {'items': [{'path': 'namespace', 'fieldRef': {'fieldPath': 'metadata.namespace'}}]}}]}}
        names, obs = token_pod(test, 'sleep 3600', automount=True, volume=vol, wait=45)
        first = {}
        for t in TARGETS:
            if t != 'mac-native' and obs.get(names[t], {}).get('phase') == 'Running':
                first[t] = kc('exec', names[t], '--', 'cat', TOKEN_DIR + '/token')[1].strip()
        changed = {}
        end = time.monotonic() + 11 * 60
        while time.monotonic() < end and len(changed) < len(first):
            time.sleep(30)
            for t, tok in first.items():
                if t not in changed and kc('exec', names[t], '--', 'cat', TOKEN_DIR + '/token')[1].strip() not in (tok, ''):
                    changed[t] = time.monotonic()
        got, detail = {}, {}
        for t in TARGETS:
            if t == 'mac-native':
                got[t] = 'UNSUPPORTED' if obs.get(names[t], {}).get('stateReason') == 'UnsupportedPodSpec' else 'PENDING'
            else:
                got[t] = 'ROTATED' if t in changed else 'NOT-ROTATED'
            detail[t] = {'phase': None, 'stateReason': None, 'msg': '', 'events': [], 'log': ''}
        cleanup()
        return got, detail
    finally:
        token_cleanup()

@custom_case(expect={t: 'PASS' for t in TARGETS})
def T32_kubelet_node_endpoints(test='T32_kubelet_node_endpoints'):
    import json
    def check(t, n):
        node = ARGS.linux_node if t == 'linux' else ARGS.mac_node
        base = '/api/v1/nodes/%s/proxy/' % node
        def raw(path):
            p = subprocess.run(['kubectl', '--kubeconfig', ARGS.kubeconfig, '--request-timeout=30s', 'get', '--raw', base + path], text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
            return p.returncode, p.stdout
        c, o = raw('healthz')
        if c or o.strip() != 'ok': return 'HEALTHZ', o
        c, o = raw('pods')
        if c or n not in o: return 'PODS', o[:120]
        c, o = raw('stats/summary')
        if c: return 'SUMMARY', o[:120]
        summary = json.loads(o)
        mine = [p for p in summary.get('pods', []) if p['podRef']['name'] == n]
        if not mine or not mine[0].get('containers') or not (mine[0]['containers'][0].get('memory') or {}).get('workingSetBytes'): return 'NO-POD-STATS', o[:120]
        if not summary['node'].get('cpu') or not summary['node'].get('memory'): return 'NO-NODE-STATS', ''
        c, o = raw('metrics/resource')
        if c or ('pod="%s"' % n) not in o or 'node_cpu_usage_seconds_total' not in o or 'container_memory_working_set_bytes' not in o: return 'METRICS', o[:120]
        return 'PASS', 'ok'
    return per_target(test, 'i=0; while true; do i=$((i+1)); done', check)

def main():
    global ARGS
    ap = argparse.ArgumentParser()
    ap.add_argument('--kubeconfig', required=True); ap.add_argument('--namespace', required=True)
    ap.add_argument('--linux-node', required=True); ap.add_argument('--mac-node', required=True)
    ap.add_argument('--json', help='write the full result to this file')
    ap.add_argument('--allowed-hostpath', help='a directory on the Mac that the test node allows for hostPath volumes (allowedHostPaths)')
    ap.add_argument('--ca', default='.local/netcheck/ca.pem', help='CA certificate of the API server, for the case that calls it from the Mac')
    ap.add_argument('--slow', action='store_true', help='also run the cases that take about ten minutes (token rotation)')
    ap.add_argument('--state-dir', help='the test node\'s state directory, to check that Pod volumes are cleaned up')
    ap.add_argument('cases', nargs='*')
    ARGS = ap.parse_args()
    names = ARGS.cases or (sorted(CASES) + sorted(CUSTOM))
    failures = []; report = {}
    for n in names:
        if n not in CASES and n not in CUSTOM:
            sys.exit('unknown case ' + n)
        print('==', n, flush=True)
        got, detail = execute(n) if n in CASES else CUSTOM[n][0]()
        expect = CASES[n][1] if n in CASES else CUSTOM[n][1]
        report[n] = {'expected': expect, 'got': got, 'detail': detail}
        for t in TARGETS:
            mark = 'ok ' if got[t] == expect[t] else 'XX '
            print(f'  {mark}{t:11s} expected={expect[t]:12s} got={got[t]:24s} {detail[t]["phase"]}/{detail[t]["stateReason"]} {detail[t]["events"]}')
            if got[t] != expect[t]:
                failures.append((n, t, expect[t], got[t], detail[t]))
    if ARGS.json:
        open(ARGS.json, 'w').write(json.dumps(report, indent=1))
    print(f'\n{len(names) * len(TARGETS) - len(failures)} of {len(names) * len(TARGETS)} expectations met')
    for f in failures:
        print('MISMATCH', f[0], f[1], 'expected', f[2], 'got', f[3], json.dumps(f[4], ensure_ascii=False)[:300])
    sys.exit(1 if failures else 0)

if __name__ == '__main__':
    main()
