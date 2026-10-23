package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestConfiguredPortsMatchWhatDockerDesktopShows(t *testing.T) {
	for name, c := range map[string]struct{ bindings, exposed, want string }{
		"published":          {`{"8080/tcp":[{"HostIp":"","HostPort":"3000"}]}`, `{"8080/tcp":{}}`, "0.0.0.0:3000->8080/tcp"},
		"bound host address": {`{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}`, `{"80/tcp":{}}`, "127.0.0.1:8080->80/tcp"},
		"several, ordered":   {`{"9000/tcp":[{"HostIp":"","HostPort":"9000"}],"80/tcp":[{"HostIp":"","HostPort":"8080"}],"53/udp":[{"HostIp":"","HostPort":"5353"}]}`, `{}`, "0.0.0.0:5353->53/udp, 0.0.0.0:8080->80/tcp, 0.0.0.0:9000->9000/tcp"},
		"declared only":      {`{}`, `{"8000/tcp":{}}`, "8000/tcp"},
		"random host port":   {`{"80/tcp":[{"HostIp":"","HostPort":""}]}`, `{"80/tcp":{}}`, "80/tcp"},
		"nothing":            {`{}`, `null`, ""},
		"invalid json":       {`nope`, `nope`, ""},
	} {
		if got := configuredPorts(c.bindings, c.exposed); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestInventoryShowsConfiguredPortsOnlyForContainersThatAreNotRunning(t *testing.T) {
	stopped := strings.Repeat("a", 64)
	running := strings.Repeat("b", 64)
	noPorts := strings.Repeat("c", 64)
	// ps lists three containers; only the stopped ones without live ports may trigger an inspect.
	fakeDocker(t, `case "$1" in
ps)
  cat <<'ROWS'
{"ID":"`+stopped+`","Names":"open-webui","Image":"img","State":"exited","Status":"Exited","Ports":"","Labels":"","ComposeProject":"","ComposeService":"","ComposeWorkingDir":"","ComposeConfigFiles":""}
{"ID":"`+running+`","Names":"ollama","Image":"img","State":"running","Status":"Up","Ports":"0.0.0.0:11432->11434/tcp","Labels":"","ComposeProject":"","ComposeService":"","ComposeWorkingDir":"","ComposeConfigFiles":""}
{"ID":"`+noPorts+`","Names":"worker","Image":"img","State":"created","Status":"Created","Ports":"","Labels":"","ComposeProject":"","ComposeService":"","ComposeWorkingDir":"","ComposeConfigFiles":""}
ROWS
  ;;
inspect)
  # Only the two containers without live ports may be asked about, never the running one.
  for last; do :; done
  case " $* " in *" `+running+` "*) echo "running container was inspected" >&2; exit 9;; esac
  printf '%s\t%s\t%s\n' "`+stopped+`" '{"8080/tcp":[{"HostIp":"","HostPort":"3000"}]}' '{"8080/tcp":{}}'
  printf '%s\t%s\t%s\n' "`+noPorts+`" '{}' '{}'
  ;;
esac
`)
	list, err := DockerInventory(context.Background())
	if err != nil || len(list) != 3 {
		t.Fatal(list, err)
	}
	by := map[string]ExternalContainer{}
	for _, c := range list {
		by[c.Name] = c
	}
	if c := by["open-webui"]; c.Ports != "0.0.0.0:3000->8080/tcp" || !c.PortsConfigured {
		t.Fatalf("stopped container: %+v", c)
	}
	if c := by["ollama"]; c.Ports != "0.0.0.0:11432->11434/tcp" || c.PortsConfigured {
		t.Fatalf("running container must keep Docker's live ports: %+v", c)
	}
	if c := by["worker"]; c.Ports != "" || c.PortsConfigured {
		t.Fatalf("container without any port: %+v", c)
	}
}

func TestInventoryKeepsWorkingWhenInspectFails(t *testing.T) {
	id := strings.Repeat("d", 64)
	fakeDocker(t, `case "$1" in
ps) echo '{"ID":"`+id+`","Names":"x","Image":"img","State":"exited","Status":"Exited","Ports":"","Labels":"","ComposeProject":"","ComposeService":"","ComposeWorkingDir":"","ComposeConfigFiles":""}' ;;
inspect) echo boom >&2; exit 1 ;;
esac
`)
	list, err := DockerInventory(context.Background())
	if err != nil || len(list) != 1 || list[0].Ports != "" {
		t.Fatalf("a failed port lookup must not break the inventory: %+v %v", list, err)
	}
}
