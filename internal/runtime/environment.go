package runtime

import (
	"os"
	"path/filepath"
	"sort"

	"maccluster/internal/envs"
)

// NewWithEnvironments is New with a registry directory used to resolve
// Spec.Environment. An empty dir disables environments.
func NewWithEnvironments(stateDir, envDir string) (Engine, error) {
	eng, err := New(stateDir)
	if err != nil {
		return nil, err
	}
	eng.(*engine).envDir = envDir
	return eng, nil
}

// Environments lists usable environment names for Node label advertisement.
func (e *engine) Environments() []string {
	if e.envDir == "" {
		return nil
	}
	list, _ := envs.List(e.envDir)
	names := make([]string, 0, len(list))
	for _, v := range list {
		names = append(names, v.Name)
	}
	sort.Strings(names)
	return names
}

// resolved holds the launch inputs derived from an environment.
type resolved struct {
	command string
	env     []string
	dir     string
}

func (e *engine) resolve(s Spec) (resolved, error) {
	r := resolved{command: s.Command[0], dir: s.WorkingDir}
	if s.Environment == "" {
		return r, nil
	}
	if e.envDir == "" {
		return r, errNoEnvironments
	}
	env, err := envs.Get(e.envDir, s.Environment)
	if err != nil {
		return r, err
	}
	if !filepath.IsAbs(r.command) {
		if r.command, err = env.LookupCommand(r.command); err != nil {
			return r, err
		}
	}
	r.env = env.Vars(os.Getenv("PATH"))
	if r.dir == "" {
		r.dir = env.WorkDir
	}
	return r, nil
}
