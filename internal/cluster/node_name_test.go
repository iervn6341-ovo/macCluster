package cluster

import "testing"

func TestNodeNamesDoNotRequireProductPrefix(t *testing.T) {
	for _, name := range []string{"my-macbook-pro", "worker01", "node.example.local", "maccluster-node"} {
		if err := ValidateNodeName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", "UPPER", "node_name", "node-", "node\n"} {
		if ValidateNodeName(name) == nil {
			t.Errorf("accepted invalid Kubernetes name %q", name)
		}
	}
}
