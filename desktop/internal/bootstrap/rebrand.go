package bootstrap

import (
	"os"
	"path/filepath"
)

// Only migration paths keep the former brand. New profiles, environment
// variables and Keychain entries use Safuno.
const legacyAppName = "DayDayUp"

// Environment prefers the new prefix, including an explicitly empty value,
// while continuing to accept existing desktop launch configurations.
func Environment(key string) string {
	if value, ok := os.LookupEnv("SAFUNO_" + key); ok {
		return value
	}
	return os.Getenv("DAYDAYUP_" + key)
}

func defaultDataDir(home string) string {
	base := filepath.Join(home, "Library", "Application Support")
	current := filepath.Join(base, appName)
	if info, err := os.Stat(filepath.Join(current, configFile)); err == nil && !info.IsDir() {
		return current
	}
	legacy := filepath.Join(base, legacyAppName)
	if info, err := os.Stat(filepath.Join(legacy, configFile)); err == nil && !info.IsDir() {
		// Reuse the existing profile in place; silently creating a new one
		// would hide accounts and can rotate encryption credentials.
		return legacy
	}
	return current
}

func readKeychainWithFallback(account string, read func(string, string) (string, error)) (string, error) {
	if value, err := read(keychain, account); err == nil && value != "" {
		return value, nil
	}
	return read(legacyAppName, account)
}
