// Package envs manages named, host-local execution environments for native
// workloads. An environment is a directory under the agent state directory that
// points at an existing toolchain (for example a Python virtualenv). It is
// configuration, not isolation: processes run with the agent's macOS account.
package envs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Names double as the key of the env.maccluster.example.com/<name> Node label,
// so they follow the Kubernetes label-name rules (lowercase, at most 63 bytes).
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_.]{0,61}[a-z0-9])?$`)

var reserved = map[string]bool{"PATH": true, "VIRTUAL_ENV": true, "MACCLUSTER_PORT": true}

const (
	TypeLink = "link" // existing virtualenv or toolchain prefix with a bin directory
	TypeDir  = "dir"  // plain directory; bin directory is optional
)

type Env struct {
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	Root      string            `json:"root"`
	BinDir    string            `json:"binDir"`
	WorkDir   string            `json:"workDir,omitempty"`
	ExtraPath []string          `json:"extraPath,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	CreatedAt string            `json:"createdAt"`
}

func ValidName(name string) bool { return namePattern.MatchString(name) }

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Add validates and stores an environment. It never creates or modifies the
// toolchain directory it points at.
func Add(dir string, e Env) error {
	if !ValidName(e.Name) {
		return fmt.Errorf("invalid environment name %q (lowercase letters, digits, '-', '_', '.'; at most 63 characters)", e.Name)
	}
	if e.Type != TypeLink && e.Type != TypeDir {
		return fmt.Errorf("unsupported environment type %q", e.Type)
	}
	if !filepath.IsAbs(e.Root) || !isDir(e.Root) {
		return errors.New("environment root must be an existing absolute directory")
	}
	if e.BinDir == "" {
		e.BinDir = filepath.Join(e.Root, "bin")
		if e.Type == TypeDir && !isDir(e.BinDir) {
			e.BinDir = e.Root
		}
	}
	if !filepath.IsAbs(e.BinDir) || !isDir(e.BinDir) {
		return errors.New("environment bin directory must be an existing absolute directory")
	}
	if e.WorkDir != "" && (!filepath.IsAbs(e.WorkDir) || !isDir(e.WorkDir)) {
		return errors.New("environment working directory must be an existing absolute directory")
	}
	for _, p := range e.ExtraPath {
		if !filepath.IsAbs(p) || !isDir(p) {
			return fmt.Errorf("extra PATH entry %q must be an existing absolute directory", p)
		}
	}
	for k := range e.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || reserved[k] {
			return fmt.Errorf("environment variable name %q is not allowed", k)
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, e.Name), 0700); err != nil {
		return err
	}
	e.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, e.Name, "env.json.tmp")
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, e.Name, "env.json"))
}

// Get returns the stored environment; a missing directory is not an error to
// create, so callers can report a clear Pending reason.
func Get(dir, name string) (Env, error) {
	var e Env
	if !ValidName(name) {
		return e, fmt.Errorf("invalid environment name %q", name)
	}
	b, err := os.ReadFile(filepath.Join(dir, name, "env.json"))
	if errors.Is(err, os.ErrNotExist) {
		return e, fmt.Errorf("environment %q is not defined on this node", name)
	}
	if err != nil {
		return e, err
	}
	if err = json.Unmarshal(b, &e); err != nil {
		return e, fmt.Errorf("environment %q: %w", name, err)
	}
	if e.Name != name {
		return e, fmt.Errorf("environment %q: stored name does not match", name)
	}
	if !isDir(e.BinDir) {
		return e, fmt.Errorf("environment %q: bin directory %s is missing", name, e.BinDir)
	}
	return e, nil
}

// List returns usable environments only; broken ones are left out so the node
// never advertises an environment that cannot start a workload.
func List(dir string) ([]Env, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Env
	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		if e, err := Get(dir, d.Name()); err == nil {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func Remove(dir, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid environment name %q", name)
	}
	if _, err := os.Stat(filepath.Join(dir, name, "env.json")); err != nil {
		return fmt.Errorf("environment %q is not defined", name)
	}
	// Only the registry entry is removed; the linked toolchain is never touched.
	return os.RemoveAll(filepath.Join(dir, name))
}

// LookupCommand resolves a bare command name inside the environment's bin
// directory. Names with path separators are rejected so a Pod cannot escape it.
func (e Env) LookupCommand(cmd string) (string, error) {
	if cmd == "" || cmd == "." || cmd == ".." || strings.ContainsRune(cmd, '/') {
		return "", fmt.Errorf("command %q must be a bare name or an absolute path", cmd)
	}
	p := filepath.Join(e.BinDir, cmd)
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode()&0111 == 0 {
		return "", fmt.Errorf("command %q not found or not executable in environment %q", cmd, e.Name)
	}
	return p, nil
}

// IsPythonVenv reports whether the root looks like a Python virtualenv. Only
// then is VIRTUAL_ENV set, so environments for other toolchains stay neutral.
func (e Env) IsPythonVenv() bool {
	_, err := os.Stat(filepath.Join(e.Root, "pyvenv.cfg"))
	return err == nil
}

// PathValue is the PATH prefix this environment contributes.
func (e Env) PathValue(path string) string {
	parts := append([]string{e.BinDir}, e.ExtraPath...)
	if path != "" {
		parts = append(parts, path)
	}
	return strings.Join(parts, ":")
}

// Vars returns KEY=VALUE entries applied before the Pod's own env.
func (e Env) Vars(path string) []string {
	out := []string{"PATH=" + e.PathValue(path), "MACCLUSTER_ENV=" + e.Name}
	if e.IsPythonVenv() {
		out = append(out, "VIRTUAL_ENV="+e.Root)
	}
	keys := make([]string, 0, len(e.Env))
	for k := range e.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+e.Env[k])
	}
	return out
}

// Status is one registry entry as shown to operators, including broken ones.
type Status struct {
	Env
	Available   bool     `json:"available"`
	Problem     string   `json:"problem,omitempty"`
	Executables []string `json:"executables,omitempty"`
}

// ListAll returns every registered environment. Unlike List it keeps broken
// entries, so the UI can explain why a node is not advertising one.
func ListAll(dir string) ([]Status, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Status{}
	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, d.Name(), "env.json")); err != nil {
			continue
		}
		e, err := Get(dir, d.Name())
		st := Status{Env: e, Available: err == nil}
		if err != nil {
			st.Name = d.Name()
			st.Problem = err.Error()
			if b, rerr := os.ReadFile(filepath.Join(dir, d.Name(), "env.json")); rerr == nil {
				json.Unmarshal(b, &st.Env)
				st.Name = d.Name()
			}
		} else {
			st.Executables = e.executables(60)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (e Env) executables(limit int) []string {
	files, err := os.ReadDir(e.BinDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, f := range files {
		if st, err := os.Stat(filepath.Join(e.BinDir, f.Name())); err == nil && st.Mode().IsRegular() && st.Mode()&0111 != 0 {
			names = append(names, f.Name())
		}
	}
	sort.Strings(names)
	if len(names) > limit {
		names = names[:limit]
	}
	return names
}
