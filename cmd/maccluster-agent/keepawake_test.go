package main

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func postSettings(t *testing.T, s *server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.settings(w, req)
	return w
}

func TestSettingsValidatePersistAndApply(t *testing.T) {
	dir := t.TempDir()
	s := &server{dir: dir, awake: newKeepAwakeLoop("")}
	s.awake.keeper.Command = func(int) *exec.Cmd { return exec.Command("sleep", "30") }
	defer s.awake.keeper.Close()
	if got := s.awake.Status(); got.Mode != "when-workloads" || got.Held {
		t.Fatalf("defaults: %+v", got)
	}
	if w := postSettings(t, s, `{"keepAwake":"sometimes"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an unknown mode got %d", w.Code)
	}
	if w := postSettings(t, s, `{"keepAwake":"always"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"held":true`) {
		t.Fatalf("always: %d %s", w.Code, w.Body.String())
	}
	if got := loadSettings(dir); got.KeepAwake != "always" {
		t.Fatalf("not persisted: %+v", got)
	}
	if w := postSettings(t, s, `{"keepAwake":"never"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"held":false`) {
		t.Fatalf("never: %d %s", w.Code, w.Body.String())
	}
	if w := postSettings(t, s, `{"unknown":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown fields must be refused: %d", w.Code)
	}
}

func TestSettingsFileWithABadValueFallsBackToTheDefault(t *testing.T) {
	dir := t.TempDir()
	if err := privateJSON(filepath.Join(dir, "agent-settings.json"), map[string]string{"keepAwake": "bogus"}); err != nil {
		t.Fatal(err)
	}
	if got := loadSettings(dir); got.KeepAwake != "" {
		t.Fatalf("a bad value must not be used: %+v", got)
	}
}
