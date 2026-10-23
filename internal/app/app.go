// Package app owns the gateway process lifecycle so it can be started by the
// CLI (cmd/gateway) and by a desktop shell without duplicating startup and
// shutdown logic.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"safuno/internal/config"
	"safuno/internal/gateway"
)

// Runtime is a running gateway instance.
type Runtime struct {
	Config  config.Config
	Gateway *gateway.Server

	server         *http.Server
	listener       net.Listener
	cancel         context.CancelFunc
	backgroundDone chan struct{}
	serveErr       chan error

	shutdownOnce sync.Once
	shutdownErr  error
}

// Start loads all gateway state and starts the HTTP server. The returned
// Runtime owns the server and must be shut down by the caller.
func Start(ctx context.Context, cfg config.Config) (*Runtime, error) {
	started := time.Now()
	// Publishing the platform names before any configuration or account is
	// validated keeps config.Registered (the validation gate) equal to the
	// compiled-in platform set even for callers that never construct a
	// gateway.
	config.SetRegisteredProviders(gateway.BuiltinProviderNames())
	s := gateway.New(cfg)
	if err := s.StoreError(); err != nil {
		return nil, fmt.Errorf("storage initialization failed: %w", err)
	}
	if err := s.RedactionError(); err != nil {
		return nil, fmt.Errorf("redaction engine failed: %w", err)
	}
	if err := s.LoadAccounts(); err != nil {
		return nil, fmt.Errorf("account store load failed: %w", err)
	}
	if err := s.LoadSessions(); err != nil {
		return nil, fmt.Errorf("session store load failed: %w", err)
	}
	if err := s.LoadAgents(); err != nil {
		return nil, fmt.Errorf("agent store load failed: %w", err)
	}
	if err := s.Pool.Load(cfg.DBFile); err != nil {
		return nil, fmt.Errorf("state load failed: %w", err)
	}
	slog.Info("startup instrumentation", "phase", "loads_done", "since_start_ms", time.Since(started).Milliseconds())

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	backgroundDone := make(chan struct{})
	go func() {
		s.Run(runCtx)
		close(backgroundDone)
	}()

	server := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	rt := &Runtime{
		Config:         cfg,
		Gateway:        s,
		server:         server,
		listener:       listener,
		cancel:         cancel,
		backgroundDone: backgroundDone,
		serveErr:       serveErr,
	}
	slog.Info("gateway listening", "address", rt.Addr(), "accounts", len(s.Pool.Accounts()))
	return rt, nil
}

// Addr returns the actual listener address, which is useful when the config
// asks for port 0.
func (r *Runtime) Addr() string {
	return r.listener.Addr().String()
}

// BaseURL is the loopback URL clients and the desktop window should use.
func (r *Runtime) BaseURL() string {
	host, port, err := net.SplitHostPort(r.Addr())
	if err != nil {
		return "http://" + r.Addr()
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// AdminURL is the built-in admin console URL.
func (r *Runtime) AdminURL() string {
	return r.BaseURL() + "/admin/"
}

// Errors reports a fatal serve error. It is buffered, so both the CLI and a
// GUI health watcher can safely observe it.
func (r *Runtime) Errors() <-chan error {
	return r.serveErr
}

// Ready waits until /healthz answers successfully or ctx expires.
func (r *Runtime) Ready(ctx context.Context) error {
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.BaseURL()+"/healthz", nil)
		if err == nil {
			resp, reqErr := client.Do(req)
			if reqErr == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-r.serveErr:
			if err == nil {
				return errors.New("gateway server stopped")
			}
			return err
		case <-ticker.C:
		}
	}
}

// Shutdown stops accepting requests, waits for in-flight work, flushes the
// redaction engine, saves state and drains the SQLite WAL. It is idempotent.
func (r *Runtime) Shutdown(ctx context.Context) error {
	r.shutdownOnce.Do(func() {
		r.cancel()
		if err := r.server.Shutdown(ctx); err != nil {
			_ = r.server.Close()
			r.shutdownErr = fmt.Errorf("shutdown failed: %w", err)
		}
		<-r.backgroundDone
		if err := r.Gateway.FlushRedaction(); err != nil {
			r.shutdownErr = errors.Join(r.shutdownErr, fmt.Errorf("final redaction checkpoint failed: %w", err))
		}
		if err := r.Gateway.Pool.Save(r.Config.DBFile); err != nil {
			r.shutdownErr = errors.Join(r.shutdownErr, fmt.Errorf("final checkpoint failed: %w", err))
		}
		// Leave the WAL drained at exit so the next boot never pays a blocking
		// checkpoint (measured ~16s on a 3.8GB database).
		if err := r.Gateway.Pool.Checkpoint(r.Config.DBFile); err != nil {
			slog.Warn("final wal checkpoint failed", "error", err)
		}
	})
	return r.shutdownErr
}
