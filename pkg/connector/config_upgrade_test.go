package connector

import (
	"strings"
	"testing"

	up "go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"
)

// TestConfigUpgradeAddsDebugDisablePrivacy reproduces the bridge's startup
// config upgrade: the embedded example is the base/output, and a pre-existing
// user config (here simulated by stripping the new key) is merged in via the
// network upgrader. The upgraded output must contain debug_disable_privacy so
// it lands in the user's config.yaml on the next restart.
func TestConfigUpgradeAddsDebugDisablePrivacy(t *testing.T) {
	if !strings.Contains(ExampleConfig, "debug_disable_privacy:") {
		t.Fatal("embedded ExampleConfig is missing debug_disable_privacy")
	}

	var base yaml.Node
	if err := yaml.Unmarshal([]byte(ExampleConfig), &base); err != nil {
		t.Fatalf("unmarshal base example: %v", err)
	}

	// Simulate an older user config that predates the key.
	oldCfg := strings.ReplaceAll(ExampleConfig, "debug_disable_privacy: false", "")
	if strings.Contains(oldCfg, "debug_disable_privacy:") {
		t.Fatal("failed to strip key from simulated old config")
	}
	var cfg yaml.Node
	if err := yaml.Unmarshal([]byte(oldCfg), &cfg); err != nil {
		t.Fatalf("unmarshal old config: %v", err)
	}

	helper := up.NewHelper(&base, &cfg)
	up.SimpleUpgrader(upgradeConfig).DoUpgrade(helper)

	out, err := yaml.Marshal(&base)
	if err != nil {
		t.Fatalf("marshal upgraded config: %v", err)
	}
	if !strings.Contains(string(out), "debug_disable_privacy:") {
		t.Errorf("upgraded config does not contain debug_disable_privacy:\n%s", out)
	}
}
