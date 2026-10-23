package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

// A successful empty query confirms absence. Inspect failure alone could mean
// an unavailable daemon, denied access, or malformed output, never an exit.
func dockerMissing(ctx context.Context, id string) bool {
	out, err := docker(ctx, "ps", "-aq", "--no-trunc", "--filter", "id="+id)
	return err == nil && out == ""
}

// Only bounded exit metadata is persisted; commands, environment and logs are
// deliberately excluded. Each UID has one atomically replaced private record.
type exitHistory struct {
	LastExitAt     string
	LastExitCode   int
	LastExitReason string
	FailureCount   int
}

func (e *engine) saveHistory(r *entry) {
	if e.dir == "" {
		return
	}
	h := exitHistory{r.instance.LastExitAt, r.instance.LastExitCode, r.instance.LastExitReason, r.instance.FailureCount}
	data, err := json.Marshal(h)
	if err != nil {
		return
	}
	path := filepath.Join(e.dir, key(r.spec.UID)+".history.json")
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, data, 0600); err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		r.instance.Message = "Unable to persist exit history: " + err.Error()
	}
}
func (e *engine) restoreHistory(r *entry) {
	data, err := os.ReadFile(filepath.Join(e.dir, key(r.spec.UID)+".history.json"))
	if err != nil || len(data) > 4096 {
		return
	}
	var h exitHistory
	if json.Unmarshal(data, &h) != nil {
		return
	}
	r.instance.LastExitAt = h.LastExitAt
	r.instance.LastExitCode = h.LastExitCode
	r.instance.LastExitReason = h.LastExitReason
	r.instance.FailureCount = h.FailureCount
}
