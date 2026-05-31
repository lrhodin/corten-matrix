package imconfig

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNetworkExampleIsValidYAML guards the embedded template itself: a
// malformed example would brick every freshly generated config.
func TestNetworkExampleIsValidYAML(t *testing.T) {
	var m map[string]any
	if err := yaml.Unmarshal([]byte(NetworkExampleConfig), &m); err != nil {
		t.Fatalf("NetworkExampleConfig is not valid YAML: %v", err)
	}
	if len(m) == 0 {
		t.Fatal("NetworkExampleConfig parsed to zero keys")
	}
}

// TestWrapNetworkParsesAndPreservesKeys is the anti-drift guard: whatever the
// bbctl path emits must (a) parse as valid YAML and (b) expose exactly the
// same key set the bridge sees, nested under `network:`. This is what stops
// the Beeper config from silently shipping a subset of keys again.
func TestWrapNetworkParsesAndPreservesKeys(t *testing.T) {
	var flat map[string]any
	if err := yaml.Unmarshal([]byte(NetworkExampleConfig), &flat); err != nil {
		t.Fatalf("flat example invalid: %v", err)
	}

	var wrapped struct {
		Network map[string]any `yaml:"network"`
	}
	if err := yaml.Unmarshal([]byte(WrapNetwork()), &wrapped); err != nil {
		t.Fatalf("WrapNetwork output is not valid YAML: %v", err)
	}
	if wrapped.Network == nil {
		t.Fatal("WrapNetwork output has no top-level `network:` key")
	}

	if len(wrapped.Network) != len(flat) {
		t.Fatalf("key count drift: flat has %d keys, wrapped network has %d", len(flat), len(wrapped.Network))
	}
	for k := range flat {
		if _, ok := wrapped.Network[k]; !ok {
			t.Errorf("key %q present in flat example but missing after WrapNetwork", k)
		}
	}
}
