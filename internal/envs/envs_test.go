package envs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func venv(t *testing.T) string {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "tool"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "bin", "data"), []byte("x"), 0644)
	os.WriteFile(filepath.Join(root, "pyvenv.cfg"), []byte("home = /usr/bin\n"), 0644)
	return root
}

func TestAddGetListRemove(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "environments")
	root := venv(t)
	if err := Add(dir, Env{Name: "vllm-metal", Type: TypeLink, Root: root, Env: map[string]string{"VLLM_PLUGINS": "metal"}}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "vllm-metal", "env.json"))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("env.json permissions: %v %v", st, err)
	}
	e, err := Get(dir, "vllm-metal")
	if err != nil || e.BinDir != filepath.Join(root, "bin") {
		t.Fatalf("%+v %v", e, err)
	}
	list, _ := List(dir)
	if len(list) != 1 || list[0].Name != "vllm-metal" {
		t.Fatalf("%+v", list)
	}
	// A link whose toolchain disappears is hidden rather than advertised.
	os.RemoveAll(root)
	if list, _ = List(dir); len(list) != 0 {
		t.Fatalf("broken environment listed: %+v", list)
	}
	if err = Remove(dir, "vllm-metal"); err != nil {
		t.Fatal(err)
	}
	if _, err = Get(dir, "vllm-metal"); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatal(err)
	}
}

func TestAddRejectsUnsafeInput(t *testing.T) {
	dir := t.TempDir()
	root := venv(t)
	for name, e := range map[string]Env{
		"bad name":     {Name: "Bad/Name", Type: TypeLink, Root: root},
		"relative":     {Name: "a", Type: TypeLink, Root: "relative"},
		"missing root": {Name: "a", Type: TypeLink, Root: filepath.Join(root, "nope")},
		"bad type":     {Name: "a", Type: "shell", Root: root},
		"reserved env": {Name: "a", Type: TypeLink, Root: root, Env: map[string]string{"PATH": "/x"}},
		"bad env key":  {Name: "a", Type: TypeLink, Root: root, Env: map[string]string{"A=B": "x"}},
		"bad workdir":  {Name: "a", Type: TypeLink, Root: root, WorkDir: "rel"},
	} {
		if err := Add(dir, e); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLookupCommandStaysInBin(t *testing.T) {
	dir := t.TempDir()
	root := venv(t)
	if err := Add(dir, Env{Name: "a", Type: TypeLink, Root: root}); err != nil {
		t.Fatal(err)
	}
	e, _ := Get(dir, "a")
	if p, err := e.LookupCommand("tool"); err != nil || p != filepath.Join(root, "bin", "tool") {
		t.Fatal(p, err)
	}
	for _, c := range []string{"../tool", "sub/tool", "data", "missing", "", ".."} {
		if _, err := e.LookupCommand(c); err == nil {
			t.Errorf("%q accepted", c)
		}
	}
	v := e.Vars("/usr/bin")
	if v[0] != "PATH="+filepath.Join(root, "bin")+":/usr/bin" || v[1] != "MACCLUSTER_ENV=a" || v[2] != "VIRTUAL_ENV="+root {
		t.Fatal(v)
	}
}

func TestNonPythonEnvironmentIsNeutral(t *testing.T) {
	dir := t.TempDir()
	root := venv(t)
	os.Remove(filepath.Join(root, "pyvenv.cfg"))
	extra := t.TempDir()
	if err := Add(dir, Env{Name: "node", Type: TypeDir, Root: root, ExtraPath: []string{extra}, Env: map[string]string{"NODE_ENV": "production"}}); err != nil {
		t.Fatal(err)
	}
	e, _ := Get(dir, "node")
	joined := strings.Join(e.Vars("/usr/bin"), "\n")
	if strings.Contains(joined, "VIRTUAL_ENV") || !strings.Contains(joined, "NODE_ENV=production") || !strings.Contains(joined, "PATH="+filepath.Join(root, "bin")+":"+extra+":/usr/bin") {
		t.Fatal(joined)
	}
	if err := Add(dir, Env{Name: "bad", Type: TypeDir, Root: root, ExtraPath: []string{"relative"}}); err == nil {
		t.Fatal("relative extra path accepted")
	}
}

func TestListAllKeepsBrokenEntries(t *testing.T) {
	dir := t.TempDir()
	good, broken := venv(t), venv(t)
	Add(dir, Env{Name: "good", Type: TypeLink, Root: good})
	Add(dir, Env{Name: "broken", Type: TypeLink, Root: broken})
	os.RemoveAll(filepath.Join(broken, "bin"))
	all, err := ListAll(dir)
	if err != nil || len(all) != 2 {
		t.Fatal(all, err)
	}
	if all[0].Name != "broken" || all[0].Available || all[0].Problem == "" || all[0].Root != broken {
		t.Fatalf("%+v", all[0])
	}
	if all[1].Name != "good" || !all[1].Available || len(all[1].Executables) != 1 || all[1].Executables[0] != "tool" {
		t.Fatalf("%+v", all[1])
	}
}

func TestShellSpecReappliesEnvironmentAfterUserRC(t *testing.T) {
	dir := t.TempDir()
	root := venv(t)
	Add(dir, Env{Name: "a", Type: TypeLink, Root: root, Env: map[string]string{"X": "1"}})
	e, _ := Get(dir, "a")
	rc := filepath.Join(t.TempDir(), "shell")
	spec, err := e.Shell(rc, "/bin/zsh", []string{"PATH=/usr/bin", "HOME=/nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Executable != "/bin/zsh" || spec.Cwd != root || spec.Args[0] != "-i" {
		t.Fatalf("%+v", spec)
	}
	b, err := os.ReadFile(filepath.Join(rc, "a", ".zshrc"))
	if err != nil || !strings.Contains(string(b), "export PATH='"+filepath.Join(root, "bin")+":/usr/bin'") || !strings.Contains(string(b), "VIRTUAL_ENV") {
		t.Fatalf("%s %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(rc, "a", ".zshrc")); st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	has := map[string]bool{}
	for _, kv := range spec.Env {
		has[kv] = true
	}
	if !has["ZDOTDIR="+filepath.Join(rc, "a")] || !has["X=1"] || !has["TERM=xterm-256color"] {
		t.Fatal(spec.Env)
	}
	if _, err = e.Shell(rc, "/nonexistent/shell", nil); err == nil {
		t.Fatal("missing shell accepted")
	}
}
