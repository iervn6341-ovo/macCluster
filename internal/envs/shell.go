package envs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ShellSpec describes an interactive shell started inside an environment.
type ShellSpec struct {
	Executable string   `json:"executable"`
	Args       []string `json:"args"`
	Env        []string `json:"env"`
	Cwd        string   `json:"cwd"`
}

// Shell prepares an interactive login-free shell whose PATH starts with the
// environment. Interactive rc files may reorder PATH (pyenv, conda, path_helper),
// so for zsh and bash a generated rc sources the user's own rc first and then
// re-applies the environment. Other shells get the plain variables only.
func (e Env) Shell(rcDir, userShell string, base []string) (ShellSpec, error) {
	if userShell == "" || !filepath.IsAbs(userShell) {
		userShell = "/bin/zsh"
	}
	if st, err := os.Stat(userShell); err != nil || st.IsDir() {
		return ShellSpec{}, fmt.Errorf("shell %q is not available", userShell)
	}
	cwd := e.WorkDir
	if cwd == "" {
		cwd = e.Root
	}
	var path string
	envMap := map[string]string{}
	for _, kv := range base {
		if k, v, ok := strings.Cut(kv, "="); ok {
			envMap[k] = v
		}
	}
	path = envMap["PATH"]
	for _, kv := range e.Vars(path) {
		k, v, _ := strings.Cut(kv, "=")
		envMap[k] = v
	}
	envMap["TERM"] = "xterm-256color"
	envMap["COLORTERM"] = "truecolor"
	if envMap["LANG"] == "" {
		envMap["LANG"] = "en_US.UTF-8"
	}
	spec := ShellSpec{Executable: userShell, Cwd: cwd}
	apply := "export PATH=" + quote(e.PathValue(path)) + "\n"
	if e.IsPythonVenv() {
		apply += "export VIRTUAL_ENV=" + quote(e.Root) + "\n"
	}
	apply += "cd " + quote(cwd) + " 2>/dev/null\n"
	dir := filepath.Join(rcDir, e.Name)
	switch filepath.Base(userShell) {
	case "zsh":
		if err := os.MkdirAll(dir, 0700); err != nil {
			return spec, err
		}
		home, _ := os.UserHomeDir()
		rc := "[ -r " + quote(filepath.Join(home, ".zshrc")) + " ] && source " + quote(filepath.Join(home, ".zshrc")) + "\n" + apply + "PROMPT=\"(" + e.Name + ") $PROMPT\"\n"
		if err := os.WriteFile(filepath.Join(dir, ".zshrc"), []byte(rc), 0600); err != nil {
			return spec, err
		}
		envMap["ZDOTDIR"] = dir
		spec.Args = []string{"-i"}
	case "bash":
		if err := os.MkdirAll(dir, 0700); err != nil {
			return spec, err
		}
		home, _ := os.UserHomeDir()
		rc := "[ -r " + quote(filepath.Join(home, ".bashrc")) + " ] && source " + quote(filepath.Join(home, ".bashrc")) + "\n" + apply + "PS1=\"(" + e.Name + ") $PS1\"\n"
		rcFile := filepath.Join(dir, "bashrc")
		if err := os.WriteFile(rcFile, []byte(rc), 0600); err != nil {
			return spec, err
		}
		spec.Args = []string{"--rcfile", rcFile, "-i"}
	default:
		spec.Args = []string{"-i"}
	}
	for k, v := range envMap {
		spec.Env = append(spec.Env, k+"="+v)
	}
	return spec, nil
}

// quote single-quotes a value for POSIX shells.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
