package bbctl

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/bridge-manager/api/beeperapi"
)

// EnvConfig holds credentials for one Beeper environment.
// Kept compatible with the upstream bridge-manager config format so that
// existing ~/.config/bbctl/config.json files continue to work.
type EnvConfig struct {
	ClusterID     string `json:"cluster_id"`
	Username      string `json:"username"`
	AccessToken   string `json:"access_token"`
	BridgeDataDir string `json:"bridge_data_dir"`
}

func (ec *EnvConfig) HasCredentials() bool {
	return strings.HasPrefix(ec.AccessToken, "syt_")
}

type EnvConfigs map[string]*EnvConfig

func (ec EnvConfigs) Get(env string) *EnvConfig {
	conf, ok := ec[env]
	if !ok {
		conf = &EnvConfig{}
		ec[env] = conf
	}
	return conf
}

type Config struct {
	DeviceID     id.DeviceID `json:"device_id"`
	Environments EnvConfigs  `json:"environments"`
	Path         string      `json:"-"`
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}

func loadConfig(path string) (*Config, error) {
	// Migrate old config path (bbctl.json → bbctl/config.json)
	baseConfigDir, _ := os.UserConfigDir()
	newDefault := filepath.Join(baseConfigDir, "bbctl", "config.json")
	oldDefault := filepath.Join(baseConfigDir, "bbctl.json")
	if path == newDefault {
		if _, err := os.Stat(oldDefault); err == nil {
			_ = os.MkdirAll(filepath.Dir(newDefault), 0700)
			_ = os.Rename(oldDefault, newDefault)
		}
	}

	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{
			DeviceID:     id.DeviceID("bbctl_" + randomHex(4)),
			Environments: make(EnvConfigs),
			Path:         path,
		}, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to open config at %s: %w", path, err)
	}
	defer file.Close()

	var cfg Config
	if err = json.NewDecoder(file).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config at %s: %w", path, err)
	}
	cfg.Path = path
	if cfg.DeviceID == "" {
		cfg.DeviceID = id.DeviceID("bbctl_" + randomHex(4))
	}
	if cfg.Environments == nil {
		cfg.Environments = make(EnvConfigs)
	}
	return &cfg, nil
}

func (cfg *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}
	file, err := os.OpenFile(cfg.Path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to open config for writing: %w", err)
	}
	defer file.Close()
	if err = json.NewEncoder(file).Encode(cfg); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}
	return nil
}

var loginCommand = &cli.Command{
	Name:   "login",
	Usage:  "Log into Beeper",
	Before: prepareApp,
	Action: cmdLogin,
}

var logoutCommand = &cli.Command{
	Name:      "logout",
	Usage:     "Delete bridge and log out of Beeper",
	ArgsUsage: "[BRIDGE]",
	Before:    requiresAuth,
	Action:    cmdLogout,
}

var whoamiCommand = &cli.Command{
	Name:    "whoami",
	Aliases: []string{"w"},
	Usage:   "Show logged-in user and bridge list",
	Before:  requiresAuth,
	Action:  cmdWhoami,
}

func readLine(prompt string) (string, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	return strings.TrimSpace(line), err
}

func cmdLogin(ctx *cli.Context) error {
	loginResp, err := beeperapi.StartLogin(baseDomain)
	if err != nil {
		return fmt.Errorf("failed to start login: %w", err)
	}

	email, err := readLine("Email: ")
	if err != nil {
		return err
	}

	if err = beeperapi.SendLoginEmail(baseDomain, loginResp.RequestID, email); err != nil {
		return fmt.Errorf("failed to send login email: %w", err)
	}

	code, err := readLine("Code (from email): ")
	if err != nil {
		return err
	}

	apiResp, err := beeperapi.SendLoginCode(baseDomain, loginResp.RequestID, code)
	if err != nil {
		return fmt.Errorf("failed to verify code: %w", err)
	}

	// Exchange the JWT login token for a Matrix access token (syt_...)
	matrixClient, err := mautrix.NewClient(fmt.Sprintf("https://matrix.%s", baseDomain), "", "")
	if err != nil {
		return fmt.Errorf("failed to create matrix client: %w", err)
	}
	cfg := getConfig(ctx)
	matrixResp, err := matrixClient.Login(ctx.Context, &mautrix.ReqLogin{
		Type:                     "org.matrix.login.jwt",
		Token:                    apiResp.LoginToken,
		DeviceID:                 cfg.DeviceID,
		InitialDeviceDisplayName: "github.com/beeper/bridge-manager",
	})
	if err != nil {
		return fmt.Errorf("failed to exchange login token: %w", err)
	}

	envCfg := cfg.Environments.Get("prod")
	envCfg.AccessToken = matrixResp.AccessToken
	whoami := apiResp.Whoami
	if whoami == nil {
		whoami, err = beeperapi.Whoami(baseDomain, matrixResp.AccessToken)
		if err != nil {
			return fmt.Errorf("failed to get user details: %w", err)
		}
	}
	// The Beeper API may not have the username ready immediately after login
	// for some accounts. Retry with backoff to handle propagation delay.
	if whoami.UserInfo.Username == "" {
		for attempt := 1; attempt <= 5; attempt++ {
			fmt.Fprintf(os.Stderr, "Waiting for username from Beeper API (attempt %d/5)...\n", attempt)
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
			whoami, err = beeperapi.Whoami(baseDomain, matrixResp.AccessToken)
			if err != nil {
				return fmt.Errorf("failed to get user details: %w", err)
			}
			if whoami.UserInfo.Username != "" {
				break
			}
		}
		if whoami.UserInfo.Username == "" {
			return fmt.Errorf("Beeper API returned empty username after login — please try again in a few seconds")
		}
	}
	envCfg.Username = whoami.UserInfo.Username
	envCfg.ClusterID = whoami.UserInfo.BridgeClusterID
	if err = cfg.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	fmt.Printf("Logged in as %s\n", envCfg.Username)
	return nil
}

func cmdLogout(ctx *cli.Context) error {
	bridge := ctx.Args().Get(0)
	if bridge == "" {
		bridge = "sh-imessage"
	}

	envCfg := getEnvConfig(ctx)
	hungryClient := getHungryClient(ctx)

	if err := hungryClient.DeleteAppService(ctx.Context, bridge); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to delete appservice: %v\n", err)
	}
	if err := beeperapi.DeleteBridge(baseDomain, bridge, envCfg.AccessToken); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to delete bridge from Beeper API: %v\n", err)
	} else {
		fmt.Printf("Bridge '%s' deleted\n", bridge)
	}

	// Invalidate the Matrix access token on the server
	if mc, err := mautrix.NewClient(fmt.Sprintf("https://matrix.%s", baseDomain), "", ""); err == nil {
		mc.AccessToken = envCfg.AccessToken
		mc.Logout(ctx.Context)
	}

	cfg := getConfig(ctx)
	username := cfg.Environments.Get("prod").Username
	cfg.Environments.Get("prod").AccessToken = ""
	cfg.Environments.Get("prod").Username = ""
	cfg.Environments.Get("prod").ClusterID = ""
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}
	fmt.Printf("Logged out of %s\n", username)
	return nil
}

func cmdWhoami(ctx *cli.Context) error {
	envCfg := getEnvConfig(ctx)
	resp, err := beeperapi.Whoami(baseDomain, envCfg.AccessToken)
	if err != nil {
		return fmt.Errorf("failed to get whoami: %w", err)
	}

	fmt.Println(resp.UserInfo.Username)
	for name, bridge := range resp.User.Bridges {
		fmt.Printf("  %s %s %s\n", name, bridge.BridgeState.BridgeType, bridge.BridgeState.StateEvent)
	}
	return nil
}
