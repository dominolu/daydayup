// Package bootstrap prepares the on-disk state and credentials used by the
// Safuno desktop shell. It is intentionally macOS-first: the first desktop
// release targets macOS, while the gateway itself stays cross-platform.
package bootstrap

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"safuno/internal/config"
	"safuno/internal/redact"
)

const (
	appName     = "Safuno"
	keychain    = "Safuno"
	envAdmin    = "GATEWAY_ADMIN_KEY"
	envGateway  = "GATEWAY_API_KEY"
	secretFile  = "secrets.json"
	configFile  = "config.json"
	keychainBin = "/usr/bin/security"
)

// Paths are the resolved locations for the desktop app's state.
type Paths struct {
	DataDir    string
	ConfigPath string
	LogPath    string
}

// Keys are the two credentials the gateway requires at startup.
type Keys struct {
	Admin   string `json:"admin_key"`
	Gateway string `json:"gateway_key"`
}

// Resolve turns the command-line overrides into absolute paths.
func Resolve(dataDirFlag, configFlag string) (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve home directory: %w", err)
	}

	dataDir := strings.TrimSpace(dataDirFlag)
	configPath := strings.TrimSpace(configFlag)

	if dataDir == "" {
		if configPath != "" {
			dataDir = filepath.Dir(configPath)
		} else {
			dataDir = defaultDataDir(home)
		}
	}
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve data directory: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return Paths{}, fmt.Errorf("create data directory: %w", err)
	}

	if configPath == "" {
		configPath = filepath.Join(dataDir, configFile)
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve config path: %w", err)
	}

	logDir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return Paths{}, fmt.Errorf("create log directory: %w", err)
	}

	return Paths{
		DataDir:    dataDir,
		ConfigPath: configPath,
		LogPath:    filepath.Join(logDir, "gateway.log"),
	}, nil
}

// LoadDotEnv loads KEY=VALUE pairs from the .env next to the config file (and
// from the data directory), without overriding variables already in the
// environment. This preserves the existing developer workflow and lets an
// existing config.json keep its account credentials.
func LoadDotEnv(configPath, dataDir string) {
	seen := map[string]bool{}
	for _, dir := range []string{filepath.Dir(configPath), dataDir} {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		loadEnvFile(filepath.Join(dir, ".env"))
	}
}

func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t\r\n") {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

// LoadOrCreateKeys returns a valid admin/gateway key pair, preferring explicit
// environment variables, then the macOS Keychain, then a 0600 fallback file.
func LoadOrCreateKeys(paths Paths) (Keys, error) {
	keys := Keys{
		Admin:   strings.TrimSpace(os.Getenv(envAdmin)),
		Gateway: strings.TrimSpace(os.Getenv(envGateway)),
	}
	stored := loadSecrets(paths)
	if keys.Admin == "" {
		keys.Admin = stored.Admin
	}
	if keys.Gateway == "" {
		keys.Gateway = stored.Gateway
	}
	if keys.Admin == "" {
		keys.Admin = generateKey()
	}
	if keys.Gateway == "" || keys.Gateway == keys.Admin {
		keys.Gateway = generateKey()
	}
	if len(keys.Admin) < 16 || len(keys.Gateway) < 16 || keys.Admin == keys.Gateway {
		return Keys{}, errors.New("gateway/admin keys must be distinct and at least 16 characters")
	}
	if err := os.Setenv(envAdmin, keys.Admin); err != nil {
		return Keys{}, fmt.Errorf("set admin key: %w", err)
	}
	if err := os.Setenv(envGateway, keys.Gateway); err != nil {
		return Keys{}, fmt.Errorf("set gateway key: %w", err)
	}
	if err := saveSecrets(paths, keys); err != nil {
		return Keys{}, err
	}
	return keys, nil
}

// EnsureConfig loads config.json, creating a sensible empty configuration on
// first launch, and makes all relative data paths absolute.
func EnsureConfig(paths Paths) (config.Config, error) {
	if _, err := os.Stat(paths.ConfigPath); errors.Is(err, fs.ErrNotExist) {
		if err := writeDefaultConfig(paths.ConfigPath, paths.DataDir); err != nil {
			return config.Config{}, err
		}
	} else if err != nil {
		return config.Config{}, fmt.Errorf("stat config: %w", err)
	}

	cfg, err := config.Load(paths.ConfigPath)
	if err != nil {
		return config.Config{}, fmt.Errorf("load config: %w", err)
	}
	return normalizePaths(cfg, filepath.Dir(paths.ConfigPath)), nil
}

func normalizePaths(cfg config.Config, base string) config.Config {
	abs := func(path string) string {
		if path == "" || filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(base, path)
	}
	cfg.DBFile = abs(cfg.DBFile)
	cfg.SessionStoreDir = abs(cfg.SessionStoreDir)
	cfg.AccountStoreFile = abs(cfg.AccountStoreFile)
	cfg.AgentStoreFile = abs(cfg.AgentStoreFile)
	cfg.StateFile = abs(cfg.StateFile)
	if cfg.Redaction != nil {
		cfg.Redaction.StoreFile = abs(cfg.Redaction.StoreFile)
	}
	if cfg.InstanceName == legacyAppName+" 网关" {
		cfg.InstanceName = "Safuno 安诺网关"
	}
	return cfg
}

func writeDefaultConfig(path, dataDir string) error {
	failClosed := true
	cfg := config.Config{
		InstanceName:           "Safuno 安诺网关",
		SessionStoreDir:        filepath.Join(dataDir, "sessions"),
		AccountStoreFile:       filepath.Join(dataDir, "accounts.enc"),
		AgentStoreFile:         filepath.Join(dataDir, "agents.json"),
		Listen:                 "127.0.0.1:8080",
		DBFile:                 filepath.Join(dataDir, "gateway.db"),
		DefaultProvider:        "sensenova",
		DefaultGroup:           "default",
		GatewayKeyEnv:          envGateway,
		AdminKeyEnv:            envAdmin,
		StateFile:              filepath.Join(dataDir, "state.json"),
		RequestTimeoutSeconds:  600,
		PoolWaitTimeoutSeconds: 120,
		RefreshIntervalSeconds: 60,
		MaxRequestBytes:        32 << 20,
		MaxAttempts:            3,
		MaxWaitingRequests:     128,
		Routing:                "round_robin",
		Accounts:               []config.Account{},
		Sessions: config.SessionsConfig{
			RetentionDays: 30,
			MaxBodyMB:     2048,
		},
		Redaction: &redact.Config{
			Enabled:          true,
			Scope:            "default",
			StoreFile:        filepath.Join(dataDir, "redaction.map.enc"),
			StoreKeyEnv:      "GATEWAY_REDACTION_STORE_KEY",
			MaxRestoreBuffer: 32 << 20,
			TokenTTLSeconds:  30 * 24 * 3600,
			FailClosed:       &failClosed,
			ToolGuard:        &redact.ToolGuard{Enabled: true, Mode: "observe"},
		},
		Agents: config.AgentsConfig{
			Enabled:    true,
			WriteMode:  "auto",
			GatewayURL: "http://127.0.0.1:8080",
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode default config: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("install config: %w", err)
	}
	return nil
}

func generateKey() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// loadSecrets reads the Keychain first and falls back to the 0600 file. It
// returns zero values when nothing is stored.
func loadSecrets(paths Paths) Keys {
	var keys Keys
	if Environment("DISABLE_KEYCHAIN") != "1" {
		if value, err := keychainGet("admin"); err == nil {
			keys.Admin = value
		}
		if value, err := keychainGet("gateway"); err == nil {
			keys.Gateway = value
		}
	}
	if keys.Admin != "" && keys.Gateway != "" {
		return keys
	}
	data, err := os.ReadFile(filepath.Join(paths.DataDir, secretFile))
	if err != nil {
		return keys
	}
	var file Keys
	if json.Unmarshal(data, &file) != nil {
		return keys
	}
	if keys.Admin == "" {
		keys.Admin = file.Admin
	}
	if keys.Gateway == "" {
		keys.Gateway = file.Gateway
	}
	return keys
}

func saveSecrets(paths Paths, keys Keys) error {
	if Environment("DISABLE_KEYCHAIN") != "1" {
		if err := saveKeychain(keys); err == nil {
			return nil
		}
	}
	path := filepath.Join(paths.DataDir, secretFile)
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return fmt.Errorf("encode secrets: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write secrets: %w", err)
	}
	_ = os.Chmod(path, 0600)
	return nil
}

func saveKeychain(keys Keys) error {
	if err := keychainSet("admin", keys.Admin); err != nil {
		return err
	}
	return keychainSet("gateway", keys.Gateway)
}

func keychainGet(account string) (string, error) {
	return readKeychainWithFallback(account, func(service, account string) (string, error) {
		out, err := exec.Command(keychainBin, "find-generic-password", "-s", service, "-a", account, "-w").Output()
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(string(out))
		if value == "" {
			return "", errors.New("empty keychain value")
		}
		return value, nil
	})
}

func keychainSet(account, value string) error {
	if _, err := os.Stat(keychainBin); err != nil {
		return err
	}
	out, err := exec.Command(keychainBin, "add-generic-password", "-U", "-s", keychain, "-a", account, "-w", value).CombinedOutput()
	if err != nil {
		return fmt.Errorf("security add-generic-password: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
