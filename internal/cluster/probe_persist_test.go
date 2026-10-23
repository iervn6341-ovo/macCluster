package cluster

import (
	"os"
	"path/filepath"
	"testing"

	runtime "maccluster/internal/runtime"
)

func TestProbeStateSurvivesAnAgentRestartForTheSameRun(t *testing.T) {
	dir := t.TempDir()
	const run = "2026-10-19T12:00:00Z"
	a := map[string]*probeState{"uid1": {}}
	st := a["uid1"]
	st.post = hookRun{runFor: run, done: true}
	st.startup.reset(run, true)
	st.readiness.reset(run, true)
	newProbeStore(dir).save(a)
	if info, err := os.Stat(filepath.Join(dir, "probe-state.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state file: %v %v", info, err)
	}

	b := map[string]*probeState{}
	newProbeStore(dir).load(b)
	got := b["uid1"]
	if got == nil || !got.post.done || got.post.runFor != run || !got.startup.ok || !got.readiness.ok {
		t.Fatalf("not restored: %+v", got)
	}
	// Same run: the hook must not run again, the readiness probe keeps its result until it is next evaluated.
	inst := runtime.Instance{StartedAt: run}
	if got.post.runFor != inst.StartedAt || got.readiness.started != inst.StartedAt {
		t.Fatal("restored state does not belong to the run")
	}
}

func TestProbeStateOfAnUnfinishedHookIsNotRestoredAsDone(t *testing.T) {
	dir := t.TempDir()
	a := map[string]*probeState{"u": {}}
	a["u"].post = hookRun{runFor: "r", done: true, err: "boom"}
	a["u"].readiness.reset("r", false)
	newProbeStore(dir).save(a)
	b := map[string]*probeState{}
	newProbeStore(dir).load(b)
	if b["u"].post.done || b["u"].readiness.ok {
		t.Fatalf("a failed hook or unready Pod must not come back as healthy: %+v", b["u"])
	}
}

func TestProbeStoreWithoutStateDirIsANoop(t *testing.T) {
	s := newProbeStore("")
	m := map[string]*probeState{"u": {}}
	s.save(m)
	s.load(m)
}
