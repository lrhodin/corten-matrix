package bbctl

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/beeper/bridge-manager/api/beeperapi"
)

var stopCommand = &cli.Command{
	Name:      "stop",
	Usage:     "Tell Beeper that the bridge is stopped (not running)",
	ArgsUsage: "BRIDGE [CONFIG_PATH]",
	Before:    requiresAuth,
	Action:    cmdStop,
}

func findBridgeConfig() (string, error) {
	dataDir := os.Getenv("XDG_DATA_HOME")
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataDir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dataDir, "corten-matrix", "config.yaml"), nil
}

func readASToken(configPath string) (string, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}
	var cfg struct {
		ASToken    string `yaml:"as_token"`
		Appservice struct {
			ASToken string `yaml:"as_token"`
		} `yaml:"appservice"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("failed to parse config: %w", err)
	}
	// bridgev2 configs nest the token under appservice; fall back to
	// legacy top-level as_token for older config shapes.
	token := cfg.Appservice.ASToken
	if token == "" {
		token = cfg.ASToken
	}
	if token == "" {
		return "", fmt.Errorf("as_token not found in config")
	}
	return token, nil
}

func cmdStop(ctx *cli.Context) error {
	if ctx.NArg() == 0 {
		return fmt.Errorf("usage: bbctl stop BRIDGE [CONFIG_PATH]")
	}
	bridge := ctx.Args().Get(0)

	// Config path: second arg, or auto-discover from XDG
	var configPath string
	if ctx.NArg() >= 2 {
		configPath = ctx.Args().Get(1)
	} else {
		var err error
		configPath, err = findBridgeConfig()
		if err != nil {
			return err
		}
	}

	asToken, err := readASToken(configPath)
	if err != nil {
		return fmt.Errorf("failed to read AS token from %s: %w", configPath, err)
	}

	envCfg := getEnvConfig(ctx)
	err = beeperapi.PostBridgeState(baseDomain, envCfg.Username, bridge, asToken, beeperapi.ReqPostBridgeState{
		StateEvent:   status.StateBridgeUnreachable,
		Reason:       "SELF_HOST_STOPPED",
		IsSelfHosted: true,
		BridgeType:   "imessage",
	})
	if err != nil {
		return fmt.Errorf("failed to post stopped state: %w", err)
	}
	fmt.Printf("Bridge '%s' stopped\n", bridge)
	return nil
}
