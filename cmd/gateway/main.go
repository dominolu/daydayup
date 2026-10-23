package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"safuno/internal/app"
	"safuno/internal/config"
)

func main() {
	bootStarted := time.Now()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	path := flag.String("config", "config.json", "configuration JSON path")
	flag.Parse()

	c, err := config.Load(*path)
	if err != nil {
		slog.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	slog.Info("startup instrumentation", "phase", "config", "since_start_ms", time.Since(bootStarted).Milliseconds())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtime, err := app.Start(ctx, c)
	if err != nil {
		slog.Error("gateway startup failed", "error", err)
		os.Exit(1)
	}

	failed := false
	select {
	case <-ctx.Done():
	case err = <-runtime.Errors():
		if err != nil {
			failed = true
			slog.Error("server failed", "error", err)
		}
		stop()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = runtime.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown failed", "error", err)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
}
