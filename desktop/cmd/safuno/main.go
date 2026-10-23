//go:build darwin

// Command safuno is the macOS desktop shell for the Safuno gateway.
//
// The shell is deliberately thin: the gateway keeps serving the same HTTP API
// and admin console, while Wails supplies the native window, tray and
// lifecycle management. The dashboard loads the gateway's own origin so the
// console's relative API calls, CSP and copyable Base URLs keep working.
package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"safuno/desktop/internal/bootstrap"
	gatewayapp "safuno/internal/app"
)

//go:embed assets
var assets embed.FS

//go:embed assets/trayTemplate.png
var trayTemplate []byte

func main() {
	dataDirFlag := flag.String("data-dir", "", "application data directory (default: ~/Library/Application Support/Safuno)")
	configFlag := flag.String("config", "", "gateway config.json path (default: <data-dir>/config.json)")
	flag.Parse()

	// `open` passes environment variables reliably even when --args is dropped;
	// use them so a LaunchServices launch can take over an existing profile.
	dataDir := *dataDirFlag
	if dataDir == "" {
		dataDir = bootstrap.Environment("DATA_DIR")
	}
	configPath := *configFlag
	if configPath == "" {
		configPath = bootstrap.Environment("CONFIG")
	}

	paths, err := bootstrap.Resolve(dataDir, configPath)
	if err != nil {
		fatal(err)
	}

	logFile, err := os.OpenFile(paths.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		fatal(fmt.Errorf("open log: %w", err))
	}
	defer logFile.Close()
	logger := slog.New(slog.NewJSONHandler(io.MultiWriter(os.Stderr, logFile), &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	slog.Info("desktop starting", "data_dir", paths.DataDir, "config", paths.ConfigPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu      sync.Mutex
		runtime *gatewayapp.Runtime
		window  *application.WebviewWindow
	)

	currentRuntime := func() *gatewayapp.Runtime {
		mu.Lock()
		defer mu.Unlock()
		return runtime
	}
	shutdownGateway := func() {
		mu.Lock()
		rt := runtime
		runtime = nil
		mu.Unlock()
		if rt == nil {
			return
		}
		shutdownCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		if err := rt.Shutdown(shutdownCtx); err != nil {
			slog.Error("gateway shutdown failed", "error", err)
		}
	}

	var wailsApp *application.App
	wailsApp = application.New(application.Options{
		Name:        "Safuno 安诺",
		Description: "Safuno 安诺模型网关",
		Logger:      logger,
		LogLevel:    slog.LevelInfo,
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(assets),
		},
		Mac: application.MacOptions{
			// Regular app: Dock icon + menu bar icon. The Dock icon is the
			// reliable entry point when macOS puts the status item on another
			// display's menu bar. Close hides the window; Quit stops the gateway.
			ActivationPolicy: application.ActivationPolicyRegular,
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID:      "dev.safuno.gateway",
			EncryptionKey: instanceKey(),
			OnSecondInstanceLaunch: func(_ application.SecondInstanceData) {
				if window != nil {
					window.Show()
					window.Focus()
				}
			},
		},
		OnShutdown: shutdownGateway,
	})

	// Single-instance detection happens inside application.New. Bootstrap only
	// after it so a second launch cannot create or rotate keys/config.
	bootstrap.LoadDotEnv(paths.ConfigPath, paths.DataDir)
	keys, err := bootstrap.LoadOrCreateKeys(paths)
	if err != nil {
		fatal(err)
	}
	cfg, err := bootstrap.EnsureConfig(paths)
	if err != nil {
		fatal(err)
	}

	rt, err := gatewayapp.Start(ctx, cfg)
	if err != nil {
		fatal(fmt.Errorf("gateway startup failed: %w", err))
	}
	mu.Lock()
	runtime = rt
	mu.Unlock()

	readyCtx, readyStop := context.WithTimeout(ctx, 20*time.Second)
	err = rt.Ready(readyCtx)
	readyStop()
	if err != nil {
		shutdownGateway()
		fatal(fmt.Errorf("gateway not ready: %w", err))
	}
	go func() {
		if err := <-rt.Errors(); err != nil {
			slog.Error("gateway server stopped", "error", err)
		}
	}()

	window = wailsApp.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "dashboard",
		Title:            "Safuno 安诺 · 模型网关管理台",
		Width:            1440,
		Height:           960,
		MinWidth:         1080,
		MinHeight:        720,
		URL:              rt.AdminURL(),
		BackgroundColour: application.NewRGB(246, 248, 250),
	})
	window.OnWindowEvent(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		window.Hide()
	})

	tray := wailsApp.SystemTray.New()
	menu := wailsApp.NewMenu()
	menu.Add("打开管理台").OnClick(func(_ *application.Context) {
		window.Show()
		window.Focus()
	})
	menu.Add("复制 Base URL").OnClick(func(_ *application.Context) {
		if rt := currentRuntime(); rt != nil {
			wailsApp.Clipboard.SetText(rt.BaseURL())
		}
	})
	menu.Add("复制管理密钥").OnClick(func(_ *application.Context) {
		wailsApp.Clipboard.SetText(keys.Admin)
	})
	menu.AddSeparator()
	menu.Add("打开数据目录").OnClick(func(_ *application.Context) {
		_ = wailsApp.Browser.OpenFile(paths.DataDir)
	})
	menu.Add("打开日志").OnClick(func(_ *application.Context) {
		_ = wailsApp.Browser.OpenFile(paths.LogPath)
	})
	menu.AddSeparator()
	menu.Add("退出 Safuno 安诺").OnClick(func(_ *application.Context) {
		wailsApp.Quit()
	})
	tray.SetMenu(menu)
	tray.SetTooltip("Safuno 安诺网关")
	tray.SetTemplateIcon(trayTemplate)
	tray.OnClick(func() {
		window.Show()
		window.Focus()
	})

	slog.Info("desktop ready", "url", rt.AdminURL())
	// A binary launched from a terminal (instead of LaunchServices) may start
	// without an active app/visible window. Wails emits ApplicationStarted once
	// the run loop is ready, so show/focus the dashboard there.
	wailsApp.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(_ *application.ApplicationEvent) {
		slog.Info("application started", "window_visible", window.IsVisible())
		window.Restore()
		window.Show()
		window.Center()
		window.Focus()
		slog.Info("window shown", "window_visible", window.IsVisible())
	})
	if err := wailsApp.Run(); err != nil {
		slog.Error("wails run failed", "error", err)
		shutdownGateway()
		os.Exit(1)
	}
	shutdownGateway()
}

func instanceKey() [32]byte {
	return sha256.Sum256([]byte("dev.safuno.gateway"))
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "safuno: %v\n", err)
	// Finder-launched apps have no terminal; show a minimal native notice with
	// the actual failure (usually a port conflict or a bad config).
	msg := err.Error()
	if runes := []rune(msg); len(runes) > 500 {
		msg = string(runes[:500]) + "…"
	}
	script := `on run argv
display dialog (item 1 of argv) with title "Safuno 安诺" buttons {"退出"} default button 1
end run`
	_ = exec.Command("/usr/bin/osascript", "-e", script, msg).Run()
	os.Exit(1)
}
