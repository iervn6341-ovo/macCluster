package cluster

import (
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	runtime "maccluster/internal/runtime"
)

func firstInt64(values ...*int64) *int64 {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func seccompOK(p *corev1.SeccompProfile) bool {
	return p == nil || p.Type == corev1.SeccompProfileTypeRuntimeDefault
}

// securityFor maps the Pod and container securityContext onto Docker options, and names what it cannot map.
// A native program runs as the macOS account of the agent in the host's filesystem, so for it only the settings
// that are already true are accepted.
func securityFor(p *corev1.Pod, c corev1.Container, executor string, add func(string, ...any)) *runtime.SecuritySpec {
	pc, cc := p.Spec.SecurityContext, c.SecurityContext
	if pc == nil && cc == nil {
		return nil
	}
	if pc != nil {
		if pc.SELinuxOptions != nil {
			add("securityContext.seLinuxOptions")
		}
		if pc.WindowsOptions != nil {
			add("securityContext.windowsOptions")
		}
		if len(pc.Sysctls) > 0 {
			add("securityContext.sysctls")
		}
		if pc.FSGroup != nil || pc.FSGroupChangePolicy != nil {
			add("securityContext.fsGroup (it only applies to volumes, which are not supported yet)")
		}
		if !seccompOK(pc.SeccompProfile) {
			add("securityContext.seccompProfile %s (only RuntimeDefault)", pc.SeccompProfile.Type)
		}
	}
	if cc != nil {
		if cc.SELinuxOptions != nil {
			add("container securityContext.seLinuxOptions")
		}
		if cc.WindowsOptions != nil {
			add("container securityContext.windowsOptions")
		}
		if cc.ProcMount != nil && *cc.ProcMount != corev1.DefaultProcMount {
			add("container securityContext.procMount")
		}
		if cc.Privileged != nil && *cc.Privileged {
			add("container securityContext.privileged: a privileged container is not supported")
		}
		if !seccompOK(cc.SeccompProfile) {
			add("container securityContext.seccompProfile %s (only RuntimeDefault)", cc.SeccompProfile.Type)
		}
	}
	var runAsUser, runAsGroup *int64
	var nonRoot *bool
	if pc != nil {
		runAsUser, runAsGroup, nonRoot = pc.RunAsUser, pc.RunAsGroup, pc.RunAsNonRoot
	}
	if cc != nil {
		runAsUser, runAsGroup = firstInt64(cc.RunAsUser, runAsUser), firstInt64(cc.RunAsGroup, runAsGroup)
		if cc.RunAsNonRoot != nil {
			nonRoot = cc.RunAsNonRoot
		}
	}
	if executor == "native" {
		if nonRoot != nil && *nonRoot && os.Getuid() == 0 {
			add("securityContext.runAsNonRoot: the agent runs as root, so a native program would too")
		}
		if runAsUser != nil && int(*runAsUser) != os.Getuid() {
			add("securityContext.runAsUser %d: a native program runs as the macOS account of the agent (uid %d)", *runAsUser, os.Getuid())
		}
		if runAsGroup != nil && int(*runAsGroup) != os.Getgid() {
			add("securityContext.runAsGroup %d: a native program runs with the group of the agent's account (gid %d)", *runAsGroup, os.Getgid())
		}
		if pc != nil && len(pc.SupplementalGroups) > 0 {
			add("securityContext.supplementalGroups")
		}
		if cc != nil {
			if cc.ReadOnlyRootFilesystem != nil && *cc.ReadOnlyRootFilesystem {
				add("securityContext.readOnlyRootFilesystem: a native program has no private filesystem")
			}
			if cc.Capabilities != nil && (len(cc.Capabilities.Add) > 0 || len(cc.Capabilities.Drop) > 0) {
				add("securityContext.capabilities: not available to native programs")
			}
		}
		return nil // allowPrivilegeEscalation: false and runAsNonRoot are already true for an unprivileged macOS account
	}
	if nonRoot != nil && *nonRoot && (runAsUser == nil || *runAsUser == 0) {
		add("securityContext.runAsNonRoot: also set runAsUser to a non-zero id (the image's default user cannot be verified here)")
	}
	sec := &runtime.SecuritySpec{}
	if runAsUser != nil {
		sec.User = strconv.FormatInt(*runAsUser, 10)
		if runAsGroup != nil {
			sec.User += ":" + strconv.FormatInt(*runAsGroup, 10)
		}
	} else if runAsGroup != nil {
		add("securityContext.runAsGroup: also set runAsUser (Docker needs both)")
	}
	if pc != nil {
		for _, g := range pc.SupplementalGroups {
			sec.GroupAdd = append(sec.GroupAdd, strconv.FormatInt(g, 10))
		}
	}
	if cc != nil {
		sec.ReadOnlyRootFS = cc.ReadOnlyRootFilesystem != nil && *cc.ReadOnlyRootFilesystem
		sec.NoNewPrivileges = cc.AllowPrivilegeEscalation != nil && !*cc.AllowPrivilegeEscalation
		if cc.Capabilities != nil {
			for _, name := range cc.Capabilities.Drop {
				sec.CapDrop = append(sec.CapDrop, capName(string(name)))
			}
			for _, name := range cc.Capabilities.Add {
				sec.CapAdd = append(sec.CapAdd, capName(string(name)))
			}
		}
	}
	if sec.User == "" && len(sec.GroupAdd) == 0 && !sec.ReadOnlyRootFS && !sec.NoNewPrivileges && len(sec.CapAdd) == 0 && len(sec.CapDrop) == 0 {
		return nil
	}
	return sec
}

// capName turns CAP_NET_RAW (the Linux spelling) and NET_RAW (Kubernetes) into what `docker --cap-add` takes.
func capName(name string) string {
	if len(name) > 4 && name[:4] == "CAP_" {
		return name[4:]
	}
	return name
}
