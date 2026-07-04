package bbctl

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/urfave/cli/v2"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/bridge-manager/api/beeperapi"
	"github.com/beeper/bridge-manager/api/hungryapi"
	"github.com/beeper/bridge-manager/bridgeconfig"

	"github.com/lrhodin/corten-matrix/pkg/imconfig"
)

var configCommand = &cli.Command{
	Name:      "config",
	Usage:     "Register a bridge and generate its configuration file",
	ArgsUsage: "BRIDGE",
	Before:    requiresAuth,
	Action:    cmdConfig,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "type",
			Aliases: []string{"t"},
			Usage:   "Bridge type (e.g. imessage-v2) — accepted but ignored, always generates iMessage config",
		},
		&cli.StringFlag{
			Name:    "output",
			Aliases: []string{"o"},
			Value:   "-",
			Usage:   "Output file path (- for stdout)",
		},
	},
}

func generateSecret(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cmdConfig(ctx *cli.Context) error {
	if ctx.NArg() == 0 {
		return fmt.Errorf("you must specify a bridge name")
	}
	bridge := ctx.Args().Get(0)

	envCfg := getEnvConfig(ctx)
	if envCfg.Username == "" {
		return fmt.Errorf("cannot generate config: username is empty (Beeper API may not have it ready yet — try again in a few seconds)")
	}
	hungryClient := getHungryClient(ctx)

	// Register (or re-fetch) the appservice with Beeper hungryserv
	reg, err := hungryClient.RegisterAppService(ctx.Context, bridge, hungryapi.ReqRegisterAppService{
		Push:       false,
		SelfHosted: true,
	})
	if err != nil {
		return fmt.Errorf("failed to register appservice: %w", err)
	}
	// Drop the extra bot user namespace entry added by hungryserv
	if len(reg.Namespaces.UserIDs) > 1 {
		reg.Namespaces.UserIDs = reg.Namespaces.UserIDs[0:1]
	}

	// Use the hunger client's domain-based URL (matrix.beeper.com) rather than
	// the direct IP from whoami.UserInfo.HungryURL — the IP endpoint uses
	// Beeper's internal CA which is not in the system trust store.
	hungryURL := hungryClient.HomeserverURL.String()

	params := bridgeconfig.Params{
		HungryAddress:      hungryURL,
		BeeperDomain:       baseDomain,
		Websocket:          true,
		AppserviceID:       reg.ID,
		ASToken:            reg.AppToken,
		HSToken:            reg.ServerToken,
		BridgeName:         bridge,
		Username:           envCfg.Username,
		UserID:             id.NewUserID(envCfg.Username, baseDomain),
		ProvisioningSecret: generateSecret(16),
		BridgeV2Name: bridgeconfig.BridgeV2Name{
			CommandPrefix:    "!im",
			// Feeds the bridgev2 template's `database.uri: file:<DatabaseFileName>.db`.
			// Must match the name used everywhere else (data dir, install-script sed,
			// the running bridge's DB) or the bridge points at a different, empty DB
			// on some runs → full re-sync + attachment re-upload loop.
			DatabaseFileName: "corten-matrix",
			BridgeTypeName:   "iMessage",
			BridgeTypeIcon:   "mxc://beeper.com/imessage",
			DefaultPickleKey: "beeper",
		},
	}

	baseConfig, err := bridgeconfig.Generate("bridgev2", params)
	if err != nil {
		return fmt.Errorf("failed to generate config: %w", err)
	}

	// Prepend the iMessage network block. This is the same embedded template
	// the bridge itself uses (pkg/imconfig), so a Beeper-generated config
	// carries the complete, current set of network keys — identical to a
	// self-hosted config. Keeping every key present is what lets the install
	// scripts skip their (fragile) key-insertion sed/python fallbacks.
	output := imconfig.WrapNetwork() + baseConfig

	// Notify Beeper that this bridge is registered and starting
	err = beeperapi.PostBridgeState(baseDomain, envCfg.Username, bridge, reg.AppToken, beeperapi.ReqPostBridgeState{
		StateEvent:   status.StateStarting,
		Reason:       "SELF_HOST_REGISTERED",
		IsSelfHosted: true,
		BridgeType:   "imessage",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to post bridge state: %v\n", err)
	}

	// Write config to file or stdout
	outputPath := ctx.String("output")
	if outputPath == "-" {
		fmt.Print(output)
	} else {
		if err = os.WriteFile(outputPath, []byte(output), 0600); err != nil {
			return fmt.Errorf("failed to write config to %s: %w", outputPath, err)
		}
		fmt.Fprintf(os.Stderr, "Config written to %s\n", outputPath)
	}

	return nil
}
