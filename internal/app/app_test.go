package app

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"daydayup/internal/config"
)

func TestStartReadyAndShutdown(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		InstanceName:           "test",
		Listen:                 "127.0.0.1:0",
		AccountStoreFile:       filepath.Join(dir, "accounts.enc"),
		AgentStoreFile:         filepath.Join(dir, "agents.json"),
		StateFile:              filepath.Join(dir, "state.json"),
		SessionStoreDir:        filepath.Join(dir, "sessions"),
		DBFile:                 filepath.Join(dir, "gateway.db"),
		DefaultProvider:        "sensenova",
		GatewayKey:             "gateway-key-0123456789",
		AdminKey:               "admin-key-0123456789",
		RequestTimeoutSeconds:  5,
		RefreshIntervalSeconds: 60,
		MaxRequestBytes:        1 << 20,
		MaxAttempts:            3,
		MaxWaitingRequests:     8,
		Accounts:               []config.Account{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := rt.Ready(readyCtx); err != nil {
		readyCancel()
		_ = rt.Shutdown(context.Background())
		t.Fatalf("ready: %v", err)
	}
	readyCancel()

	resp, err := http.Get(rt.BaseURL() + "/healthz")
	if err != nil {
		t.Fatalf("healthz request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, body = %s", resp.StatusCode, body)
	}

	admin, err := http.Get(rt.AdminURL())
	if err != nil {
		t.Fatalf("admin request: %v", err)
	}
	adminBody, _ := io.ReadAll(admin.Body)
	admin.Body.Close()
	if admin.StatusCode != http.StatusOK {
		t.Fatalf("admin status = %d", admin.StatusCode)
	}
	if len(adminBody) == 0 {
		t.Fatal("admin page is empty")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := rt.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// Shutdown is idempotent.
	if err := rt.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}
