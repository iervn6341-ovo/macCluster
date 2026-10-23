package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDockerCLI records the arguments and standard input of each call, and fails when told to.
func fakeDockerCLI(t *testing.T, failWith string) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"ARGS: $@\" >> " + calls + "\ncat >> " + calls + " 2>/dev/null\n"
	if failWith != "" {
		script += "echo '" + failWith + "' >&2\nexit 1\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return calls
}

func TestLoginSendsThePasswordOnStandardInputOnly(t *testing.T) {
	calls := fakeDockerCLI(t, "")
	err := dockerLoginRun(context.Background(), dockerLoginRequest{Registry: "registry.example.com:5000", Username: "alice", Password: "p@ss w0rd"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(calls)
	text := string(b)
	if !strings.Contains(text, "ARGS: login --username alice --password-stdin registry.example.com:5000") {
		t.Fatalf("arguments: %q", text)
	}
	if strings.Count(text, "p@ss w0rd") != 1 || strings.Contains(strings.SplitN(text, "\n", 2)[0], "p@ss") {
		t.Fatalf("the password must arrive on standard input and nowhere else: %q", text)
	}
	// Docker Hub: no registry argument.
	os.Remove(calls)
	if err := dockerLoginRun(context.Background(), dockerLoginRequest{Username: "u", Password: "x"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(calls)
	if !strings.Contains(string(b), "ARGS: login --username u --password-stdin\n") {
		t.Fatalf("hub login: %q", b)
	}
}

func TestLoginRefusesWhatItCannotSendSafely(t *testing.T) {
	calls := fakeDockerCLI(t, "")
	for name, req := range map[string]dockerLoginRequest{
		"no user":            {Username: "", Password: "x"},
		"no password":        {Username: "u", Password: ""},
		"newline password":   {Username: "u", Password: "a\nb"},
		"bad registry":       {Registry: "reg; rm -rf /", Username: "u", Password: "x"},
		"registry with flag": {Registry: "--config=/tmp/x", Username: "u", Password: "x"},
		"user with newline":  {Username: "u\nv", Password: "x"},
	} {
		if err := dockerLoginRun(context.Background(), req); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := os.Stat(calls); err == nil {
		t.Fatal("docker was called for a request that should have been refused")
	}
}

func TestLoginErrorNeverEchoesTheSecret(t *testing.T) {
	fakeDockerCLI(t, "Error response from daemon: Get https://r/v2/: unauthorized: incorrect username or password (token S3CRET-TOKEN)")
	err := dockerLoginRun(context.Background(), dockerLoginRequest{Registry: "r.example.com", Username: "u", Password: "S3CRET-TOKEN"})
	if err == nil || strings.Contains(err.Error(), "S3CRET-TOKEN") || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("the message must help and must not contain the secret: %v", err)
	}
}

func TestLoginsListsRegistryNamesOnly(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(`{"auths":{"https://index.docker.io/v1/":{},"registry.example.com":{"auth":"dXNlcjpzZWNyZXQ="}},"credsStore":"desktop","credHelpers":{"gcr.io":"gcloud"}}`), 0o600)
	list, err := dockerLogins(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Registry != "gcr.io" || list[0].Store != "gcloud" || list[1].Registry != "https://index.docker.io/v1/" || list[1].Store != "desktop" {
		t.Fatalf("%+v", list)
	}
	for _, l := range list {
		if strings.Contains(l.Registry+l.Store, "dXNlcjpzZWNyZXQ") {
			t.Fatal("a credential was returned")
		}
	}
	if none, err := dockerLogins(filepath.Join(dir, "missing.json")); err != nil || len(none) != 0 {
		t.Fatalf("no config file means no logins: %v %v", none, err)
	}
	os.WriteFile(cfg, []byte("not json"), 0o600)
	if _, err := dockerLogins(cfg); err == nil {
		t.Fatal("a broken config file was accepted")
	}
}

func TestLogout(t *testing.T) {
	calls := fakeDockerCLI(t, "")
	if err := dockerLogoutRun(context.Background(), "registry.example.com"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(calls)
	if !strings.Contains(string(b), "ARGS: logout registry.example.com") {
		t.Fatalf("%q", b)
	}
	if dockerLogoutRun(context.Background(), "bad host") == nil {
		t.Fatal("bad registry accepted")
	}
}

func TestKeychainFailureGetsAnExplanation(t *testing.T) {
	fakeDockerCLI(t, "error saving credentials: error storing credentials - err: exit status 1, out: `User interaction is not allowed. (-25308)`")
	err := dockerLoginRun(context.Background(), dockerLoginRequest{Username: "u", Password: "x"})
	if err == nil || !strings.Contains(err.Error(), "鑰匙圈") || !strings.Contains(err.Error(), "-25308") {
		t.Fatalf("the user needs to know what to do: %v", err)
	}
}
