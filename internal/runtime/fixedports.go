package runtime

import (
	"fmt"
	"net"
	"strconv"
	"strings"
)

const maxFixedPorts = 8

// portBusyError means a fixed port is not available yet. It is transient: the
// workload stays Pending and is retried, it is not counted as a failure.
type portBusyError struct {
	port   int
	holder string
}

func (e *portBusyError) Error() string {
	if e.holder != "" {
		return fmt.Sprintf("fixed port %d is already in use by %s", e.port, e.holder)
	}
	return fmt.Sprintf("fixed port %d is already in use", e.port)
}

func joinPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

// checkFixedPorts refuses to start when another workload of this engine owns one
// of the ports, or when anything on the host is already listening on it. The
// check cannot remove the window between it and the program's own bind.
func (e *engine) checkFixedPorts(self *entry, ports []int) error {
	for _, p := range ports {
		for uid, other := range e.entries {
			if other == self || uid == self.spec.UID || other.exited || (other.cmd == nil && other.adoptPID == 0) {
				continue
			}
			for _, q := range other.spec.FixedPorts {
				if q == p {
					name := other.instance.Name
					if name == "" {
						name = uid
					}
					return &portBusyError{port: p, holder: name}
				}
			}
		}
		// Listen on all interfaces: the program may bind 0.0.0.0, and a bind on
		// any interface conflicts with a listener on another one.
		l, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
		if err != nil {
			return &portBusyError{port: p}
		}
		l.Close()
	}
	return nil
}
