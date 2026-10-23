package bootstrap

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRebrandEnvironmentPreservesExistingLaunchConfiguration(t *testing.T) {
	t.Setenv("SAFUNO_REBRAND_TEST", "")
	if err := os.Unsetenv("SAFUNO_REBRAND_TEST"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DAYDAYUP_REBRAND_TEST", "legacy-profile")
	if got := Environment("REBRAND_TEST"); got != "legacy-profile" {
		t.Fatalf("legacy environment = %q", got)
	}
	t.Setenv("SAFUNO_REBRAND_TEST", "current-profile")
	if got := Environment("REBRAND_TEST"); got != "current-profile" {
		t.Fatalf("current environment = %q", got)
	}
	t.Setenv("SAFUNO_REBRAND_TEST", "")
	if got := Environment("REBRAND_TEST"); got != "" {
		t.Fatalf("explicit empty override = %q", got)
	}
}

func TestRebrandResolveKeepsExistingProfilesAndExplicitOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := filepath.Join(home, "Library", "Application Support")
	current := filepath.Join(base, appName)
	legacy := filepath.Join(base, legacyAppName)
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, configFile), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	paths, err := Resolve("", "")
	if err != nil {
		t.Fatal(err)
	}
	if paths.DataDir != legacy {
		t.Fatalf("existing profile lost: %q", paths.DataDir)
	}
	if err := os.MkdirAll(current, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, configFile), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	paths, err = Resolve("", "")
	if err != nil {
		t.Fatal(err)
	}
	if paths.DataDir != current {
		t.Fatalf("current profile not preferred: %q", paths.DataDir)
	}
	explicit := t.TempDir()
	paths, err = Resolve(explicit, "")
	if err != nil {
		t.Fatal(err)
	}
	if paths.DataDir != explicit {
		t.Fatalf("explicit profile overridden: %q", paths.DataDir)
	}
}

func TestRebrandNewInstallUsesSafunoProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	paths, err := Resolve("", "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Library", "Application Support", "Safuno")
	if paths.DataDir != want {
		t.Fatalf("data directory = %q, want %q", paths.DataDir, want)
	}
}

func TestRebrandKeychainPreservesExistingCredentials(t *testing.T) {
	for _, legacyOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-service", true: "legacy-service"}[legacyOnly], func(t *testing.T) {
			var services []string
			got, err := readKeychainWithFallback("admin", func(service, account string) (string, error) {
				services = append(services, service)
				if account != "admin" {
					t.Fatalf("unexpected account %q", account)
				}
				if legacyOnly && service == keychain {
					return "", errors.New("not found")
				}
				return service + "-existing-secret", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			service := keychain
			wantCalls := 1
			if legacyOnly {
				service = legacyAppName
				wantCalls = 2
			}
			if got != service+"-existing-secret" || len(services) != wantCalls {
				t.Fatalf("credential lookup = %q, services %v", got, services)
			}
		})
	}
}

func TestRebrandKeepsFallbackFileKeys(t *testing.T) {
	t.Setenv("SAFUNO_DISABLE_KEYCHAIN", "1")
	t.Setenv(envAdmin, "")
	t.Setenv(envGateway, "")
	paths := Paths{DataDir: t.TempDir()}
	want := Keys{Admin: "existing-admin-key-1234567890", Gateway: "existing-gateway-key-1234567890"}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(paths.DataDir, secretFile)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOrCreateKeys(paths)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatal("existing credentials rotated after rebrand")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("secrets mode = %v", info.Mode().Perm())
	}
}
