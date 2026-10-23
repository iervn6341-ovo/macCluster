package runtime

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// validateMounts refuses what `docker create` would misread or what could escape its meaning. `--mount` is a
// comma-separated list, so a path with a comma, quote or newline cannot be passed safely and is refused.
func validateMounts(mounts []Mount) error {
	seen := map[string]bool{}
	for _, m := range mounts {
		if m.Name == "" {
			return fmt.Errorf("a mount has no name")
		}
		if !filepath.IsAbs(m.Container) || filepath.Clean(m.Container) != m.Container || m.Container == "/" {
			return fmt.Errorf("volume %q: container path %q must be an absolute, clean path below /", m.Name, m.Container)
		}
		if seen[m.Container] {
			return fmt.Errorf("two volumes are mounted at %s", m.Container)
		}
		seen[m.Container] = true
		if strings.ContainsAny(m.Container, ",\"\n\r") {
			return fmt.Errorf("volume %q: container path %q has a character Docker cannot take in a mount", m.Name, m.Container)
		}
		switch {
		case m.Tmpfs:
			if m.HostPath != "" || m.ReadOnly {
				return fmt.Errorf("volume %q: a memory volume has no host path and is writable", m.Name)
			}
			if m.SizeBytes < 0 {
				return fmt.Errorf("volume %q: invalid size", m.Name)
			}
		default:
			if !filepath.IsAbs(m.HostPath) || filepath.Clean(m.HostPath) != m.HostPath {
				return fmt.Errorf("volume %q: host path %q must be an absolute, clean path", m.Name, m.HostPath)
			}
			if strings.ContainsAny(m.HostPath, ",\"\n\r") {
				return fmt.Errorf("volume %q: host path %q has a comma, quote or newline, which Docker cannot take in a mount", m.Name, m.HostPath)
			}
		}
	}
	return nil
}

// dockerMountArgs turns mounts into `--mount`/`--tmpfs` arguments. `--mount` is used for bind mounts because `-v`
// stumbles over colons and spaces in paths such as "~/Library/Application Support/...".
func dockerMountArgs(mounts []Mount) []string {
	var args []string
	for _, m := range mounts {
		if m.Tmpfs {
			opt := m.Container
			if m.SizeBytes > 0 {
				opt += ":size=" + strconv.FormatInt(m.SizeBytes, 10)
			}
			args = append(args, "--tmpfs", opt)
			continue
		}
		spec := "type=bind,src=" + m.HostPath + ",dst=" + m.Container
		if m.ReadOnly {
			spec += ",readonly"
		}
		args = append(args, "--mount", spec)
	}
	return args
}
