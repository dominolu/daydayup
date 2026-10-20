package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"daydayup/internal/config"
	"daydayup/internal/gateway"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	path := flag.String("config", "config.json", "configuration JSON path")
	flag.Parse()
	c, err := config.Load(*path)
	if err != nil {
		slog.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	s := gateway.New(c)
	if err = s.StoreError(); err != nil {
		slog.Error("storage initialization failed", "error", err)
		os.Exit(1)
	}
	if err = s.RedactionError(); err != nil {
		slog.Error("redaction engine failed", "error", err)
		os.Exit(1)
	}
	if err = s.LoadAccounts(); err != nil {
		slog.Error("account store load failed", "error", err)
		os.Exit(1)
	}
	if err = s.LoadSessions(); err != nil {
		slog.Error("session store load failed", "error", err)
		os.Exit(1)
	}
	if err = s.LoadAgents(); err != nil {
		slog.Error("agent store load failed", "error", err)
		os.Exit(1)
	}
	if err = s.Pool.Load(c.DBFile); err != nil {
		slog.Error("state load failed", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backgroundDone := make(chan struct{})
	go func() { s.Run(ctx); close(backgroundDone) }()
	server := &http.Server{Addr: c.Listen, Handler: s, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("gateway listening", "address", c.Listen, "accounts", len(s.Pool.Accounts()))
		serverErrors <- server.ListenAndServe()
	}()
	failed := false
	select {
	case <-ctx.Done():
	case err = <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			failed = true
			slog.Error("server failed", "error", err)
		}
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err = server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
		slog.Error("shutdown failed", "error", err)
	}
	<-backgroundDone
	if err = s.FlushRedaction(); err != nil {
		slog.Error("final redaction checkpoint failed", "error", err)
		failed = true
	}
	if err = s.Pool.Save(c.DBFile); err != nil {
		slog.Error("final checkpoint failed", "error", err)
		failed = true
	}
	if failed {
		os.Exit(1)
	}
}
