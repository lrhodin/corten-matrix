package bbctl

import (
	"fmt"
	"os"

	"github.com/urfave/cli/v2"

	"github.com/beeper/bridge-manager/api/beeperapi"
)

var deleteCommand = &cli.Command{
	Name:      "delete",
	Usage:     "Delete a bridge registration",
	ArgsUsage: "BRIDGE",
	Before:    requiresAuth,
	Action:    cmdDelete,
}

func cmdDelete(ctx *cli.Context) error {
	if ctx.NArg() == 0 {
		return fmt.Errorf("you must specify a bridge name")
	}
	bridge := ctx.Args().Get(0)

	envCfg := getEnvConfig(ctx)
	hungryClient := getHungryClient(ctx)

	if err := hungryClient.DeleteAppService(ctx.Context, bridge); err != nil {
		return fmt.Errorf("failed to delete appservice: %w", err)
	}

	if err := beeperapi.DeleteBridge(baseDomain, bridge, envCfg.AccessToken); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to delete bridge from Beeper API: %v\n", err)
	}

	fmt.Printf("Bridge '%s' deleted\n", bridge)
	return nil
}
