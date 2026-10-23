package runtime

import (
	"reflect"
	"strings"
	"testing"
)

func TestDockerMountArgs(t *testing.T) {
	got := dockerMountArgs([]Mount{
		{Name: "data", HostPath: "/path/to/Application Support/MacCluster/volumes/u1/data", Container: "/data"},
		{Name: "cfg", HostPath: "/h/cfg", Container: "/etc/cfg", ReadOnly: true},
		{Name: "scratch", Container: "/scratch", Tmpfs: true, SizeBytes: 8 << 20},
		{Name: "ram", Container: "/ram", Tmpfs: true},
	})
	want := []string{
		"--mount", "type=bind,src=/path/to/Application Support/MacCluster/volumes/u1/data,dst=/data",
		"--mount", "type=bind,src=/h/cfg,dst=/etc/cfg,readonly",
		"--tmpfs", "/scratch:size=8388608",
		"--tmpfs", "/ram",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestValidateMountsRefusesWhatDockerWouldMisread(t *testing.T) {
	ok := []Mount{{Name: "a", HostPath: "/h/a", Container: "/a"}, {Name: "m", Container: "/m", Tmpfs: true, SizeBytes: 1}}
	if err := validateMounts(ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]Mount{
		"relative container path": {{Name: "a", HostPath: "/h/a", Container: "a"}},
		"root":                    {{Name: "a", HostPath: "/h/a", Container: "/"}},
		"unclean path":            {{Name: "a", HostPath: "/h/a", Container: "/a/../b"}},
		"duplicate target":        {{Name: "a", HostPath: "/h/a", Container: "/x"}, {Name: "b", HostPath: "/h/b", Container: "/x"}},
		"comma in host path":      {{Name: "a", HostPath: "/h/a,b", Container: "/a"}},
		"quote in host path":      {{Name: "a", HostPath: "/h/a\"b", Container: "/a"}},
		"relative host path":      {{Name: "a", HostPath: "h/a", Container: "/a"}},
		"tmpfs with host path":    {{Name: "a", HostPath: "/h", Container: "/a", Tmpfs: true}},
		"readonly tmpfs":          {{Name: "a", Container: "/a", Tmpfs: true, ReadOnly: true}},
		"no name":                 {{HostPath: "/h/a", Container: "/a"}},
	}
	for name, mounts := range bad {
		if err := validateMounts(mounts); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestMountsAreDockerOnly(t *testing.T) {
	s := Spec{UID: "u", Executor: "native", Command: []string{"/bin/true"}, Mounts: []Mount{{Name: "a", HostPath: "/h", Container: "/a"}}}
	if err := validate(&s); err == nil || !strings.Contains(err.Error(), "Docker") {
		t.Fatalf("a native program must refuse mounts: %v", err)
	}
}
