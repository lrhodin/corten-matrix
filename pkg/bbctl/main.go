package bbctl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/beeper/bridge-manager/api/beeperapi"
	"github.com/beeper/bridge-manager/api/hungryapi"
)

const baseDomain = "beeper.com"

type contextKey int

const (
	contextKeyConfig contextKey = iota
	contextKeyEnvConfig
	contextKeyHungryClient
)

func getConfig(ctx *cli.Context) *Config {
	return ctx.Context.Value(contextKeyConfig).(*Config)
}

func getEnvConfig(ctx *cli.Context) *EnvConfig {
	return ctx.Context.Value(contextKeyEnvConfig).(*EnvConfig)
}

func getHungryClient(ctx *cli.Context) *hungryapi.Client {
	val := ctx.Context.Value(contextKeyHungryClient)
	if val == nil {
		return nil
	}
	return val.(*hungryapi.Client)
}

func getConfigPath() string {
	baseDir, _ := os.UserConfigDir()
	return filepath.Join(baseDir, "bbctl", "config.json")
}

func prepareApp(ctx *cli.Context) error {
	cfg, err := loadConfig(ctx.String("config"))
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	envCfg := cfg.Environments.Get("prod")
	newCtx := context.WithValue(ctx.Context, contextKeyConfig, cfg)
	newCtx = context.WithValue(newCtx, contextKeyEnvConfig, envCfg)
	if envCfg.HasCredentials() {
		if envCfg.Username == "" {
			fmt.Fprintf(os.Stderr, "Fetching whoami to fill missing username...\n")
			for attempt := 0; attempt < 5; attempt++ {
				if attempt > 0 {
					fmt.Fprintf(os.Stderr, "  Retrying (%d/5)...\n", attempt+1)
					time.Sleep(time.Duration(attempt) * 2 * time.Second)
				}
				resp, whoamiErr := beeperapi.Whoami(baseDomain, envCfg.AccessToken)
				if whoamiErr != nil {
					return fmt.Errorf("failed to get whoami: %w", whoamiErr)
				}
				envCfg.Username = resp.UserInfo.Username
				envCfg.ClusterID = resp.UserInfo.BridgeClusterID
				if envCfg.Username != "" {
					break
				}
			}
			if envCfg.Username != "" {
				if saveErr := cfg.Save(); saveErr != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to save config: %v\n", saveErr)
				}
			} else {
				fmt.Fprintf(os.Stderr, "Warning: Beeper API returned empty username\n")
			}
		}
		hungryClient := hungryapi.NewClient(baseDomain, envCfg.Username, envCfg.AccessToken)
		newCtx = context.WithValue(newCtx, contextKeyHungryClient, hungryClient)
	}
	ctx.Context = newCtx
	return nil
}

func requiresAuth(ctx *cli.Context) error {
	if err := prepareApp(ctx); err != nil {
		return err
	}
	if !getEnvConfig(ctx).HasCredentials() {
		return fmt.Errorf("you are not logged in — run 'bbctl login' first")
	}
	return nil
}

func Run(args []string) {
	app := &cli.App{
		Name:    "bbctl",
		Usage:   "Manage self-hosted Beeper bridges",
		Version: "0.1.0",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "config",
				Usage: "Path to config file",
				Value: getConfigPath(),
			},
		},
		Commands: []*cli.Command{
			loginCommand,
			logoutCommand,
			whoamiCommand,
			configCommand,
			stopCommand,
			deleteCommand,
		},
	}
	if err := app.Run(args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
