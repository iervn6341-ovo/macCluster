package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func fakeEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
func noFile(string) ([]byte, error)                   { return nil, errors.New("no file") }
func host() (string, error)                           { return "host-1", nil }

func TestControllerFlagsDefaultsAndPodIdentity(t *testing.T) {
	cfg, err := parseControllerArgs([]string{"--namespaces", "a, b"}, fakeEnv(map[string]string{"POD_NAME": "ctl-1", "POD_NAMESPACE": "maccluster-system"}), noFile, host)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.namespaces, ",") != "a,b" || !cfg.elect || cfg.election.Identity != "ctl-1" || cfg.election.Namespace != "maccluster-system" {
		t.Fatalf("%+v", cfg)
	}
	if cfg.election.LeaseDuration != 15*time.Second || cfg.election.RenewDeadline != 10*time.Second || cfg.election.RetryPeriod != 2*time.Second || cfg.interval != 3*time.Second {
		t.Fatalf("defaults: %+v", cfg)
	}
}

func TestControllerFlagsFallBackToTheServiceAccountNamespaceAndHostname(t *testing.T) {
	read := func(p string) ([]byte, error) {
		if strings.HasSuffix(p, "/namespace") {
			return []byte("from-file\n"), nil
		}
		return nil, errors.New("x")
	}
	cfg, err := parseControllerArgs([]string{"--namespaces", "a"}, fakeEnv(nil), read, host)
	if err != nil || cfg.election.Namespace != "from-file" || cfg.election.Identity != "host-1" {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestControllerFlagsRefuseWhatCannotWork(t *testing.T) {
	env := fakeEnv(map[string]string{"POD_NAME": "p", "POD_NAMESPACE": "n"})
	for name, args := range map[string][]string{
		"no namespaces":      {},
		"empty entry":        {"--namespaces", "a,,b"},
		"interval too short": {"--namespaces", "a", "--interval", "100ms"},
		"bad timings":        {"--namespaces", "a", "--lease-duration", "5s", "--renew-deadline", "10s"},
		"unknown flag":       {"--namespaces", "a", "--bogus"},
	} {
		if _, err := parseControllerArgs(args, env, noFile, host); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	// Without a namespace for the Lease anywhere, election cannot work: say how to fix it.
	_, err := parseControllerArgs([]string{"--namespaces", "a"}, fakeEnv(nil), noFile, host)
	if err == nil || !strings.Contains(err.Error(), "--lease-namespace") {
		t.Fatalf("got %v", err)
	}
	// Election off (a single local run) needs neither.
	if _, err := parseControllerArgs([]string{"--namespaces", "a", "--leader-elect=false"}, fakeEnv(nil), noFile, host); err != nil {
		t.Fatalf("without election: %v", err)
	}
}
