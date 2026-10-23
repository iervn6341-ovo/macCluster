package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Docker registry logins for the app's settings page. The agent only drives the `docker` CLI: the credentials are
// kept by Docker's own credential store (the macOS Keychain through Docker Desktop's helper), never by macCluster,
// and the password reaches `docker login` on standard input, never on a command line or in a file of ours.

type dockerLoginRequest struct {
	Registry string `json:"registry"` // empty: Docker Hub
	Username string `json:"username"`
	Password string `json:"password"`
}

type dockerLogin struct {
	Registry string `json:"registry"`
	Store    string `json:"store,omitempty"` // the credential helper holding the secret ("desktop", "osxkeychain"), "" = Docker's config file
}

var registryPattern = regexp.MustCompile(`^(https?://)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)

func validRegistry(r string) bool {
	return r == "" || (len(r) <= 255 && registryPattern.MatchString(r))
}

// dockerConfigPath is the config file `docker` itself uses.
func dockerConfigPath() string {
	if d := os.Getenv("DOCKER_CONFIG"); d != "" {
		return filepath.Join(d, "config.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".docker", "config.json")
}

// dockerEnv adds the places Docker Desktop keeps `docker` and its credential helpers: a LaunchAgent starts with a
// minimal PATH.
func dockerPath() string {
	home, _ := os.UserHomeDir()
	extra := []string{"/opt/homebrew/bin", "/usr/local/bin", "/Applications/Docker.app/Contents/Resources/bin", filepath.Join(home, ".docker", "bin"), "/usr/bin", "/bin"}
	return os.Getenv("PATH") + ":" + strings.Join(extra, ":")
}

func runDocker(ctx context.Context, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), "PATH="+dockerPath())
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	text := strings.TrimSpace(out.String() + "\n" + errb.String())
	if err != nil {
		if ctx.Err() != nil {
			return "", errors.New("docker did not answer in time")
		}
		return "", errors.New(cleanDockerMessage(text, stdin))
	}
	return text, nil
}

// cleanDockerMessage keeps what helps the user and drops the rest, in particular anything that is the secret.
func cleanDockerMessage(text, secret string) string {
	if secret != "" {
		text = strings.ReplaceAll(text, strings.TrimSpace(secret), "***")
	}
	lines := []string{}
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "WARNING!") || strings.Contains(l, "credentials-store") || strings.Contains(l, "docs.docker.com") {
			continue
		}
		lines = append(lines, l)
	}
	msg := strings.Join(lines, " ")
	if len(msg) > 300 {
		msg = msg[:300]
	}
	if strings.Contains(msg, "User interaction is not allowed") || strings.Contains(msg, "-25308") {
		msg += "（無法使用 macOS 鑰匙圈：可能沒有在登入的桌面階段執行，或鑰匙圈已鎖定。請解鎖鑰匙圈後再試，或在終端機直接執行 docker login。）"
	}
	if msg == "" {
		return "docker reported an error"
	}
	return msg
}

func dockerLoginRun(ctx context.Context, req dockerLoginRequest) error {
	if !validRegistry(req.Registry) {
		return fmt.Errorf("the registry address is not valid")
	}
	if strings.TrimSpace(req.Username) == "" || strings.ContainsAny(req.Username, "\r\n\x00") {
		return fmt.Errorf("a user name is required")
	}
	if req.Password == "" || strings.ContainsAny(req.Password, "\r\n\x00") {
		return fmt.Errorf("a password or access token is required (one line)")
	}
	args := []string{"login", "--username", req.Username, "--password-stdin"}
	if req.Registry != "" {
		args = append(args, req.Registry)
	}
	_, err := runDocker(ctx, req.Password+"\n", args...)
	return err
}

func dockerLogoutRun(ctx context.Context, registry string) error {
	if !validRegistry(registry) {
		return fmt.Errorf("the registry address is not valid")
	}
	args := []string{"logout"}
	if registry != "" {
		args = append(args, registry)
	}
	_, err := runDocker(ctx, "", args...)
	return err
}

// dockerLogins lists the registries Docker has a login for. Only names are read: the values in `auths` (a base64 of
// the password when no helper is used) are never looked at, copied or returned.
func dockerLogins(configPath string) ([]dockerLogin, error) {
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		return []dockerLogin{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Auths       map[string]json.RawMessage `json:"auths"`
		CredsStore  string                     `json:"credsStore"`
		CredHelpers map[string]string          `json:"credHelpers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("docker's config file is not valid JSON")
	}
	seen := map[string]dockerLogin{}
	for reg := range cfg.Auths {
		seen[reg] = dockerLogin{Registry: reg, Store: cfg.CredsStore}
	}
	for reg, helper := range cfg.CredHelpers {
		seen[reg] = dockerLogin{Registry: reg, Store: helper}
	}
	out := make([]dockerLogin, 0, len(seen))
	for _, l := range seen {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Registry < out[j].Registry })
	return out, nil
}
