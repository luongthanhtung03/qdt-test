// Command server runs the content management service.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/luongthanhtung03/qdt-test/internal/clock"
	"github.com/luongthanhtung03/qdt-test/internal/config"
	"github.com/luongthanhtung03/qdt-test/internal/httpapi"
	"github.com/luongthanhtung03/qdt-test/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("startup failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// Logging is not configured yet, so write plainly.
		return err
	}
	setupLogging(cfg)

	// Identifies this process when it takes a lease on a scheduled job.
	instanceID := uuid.NewString()
	slog.Info("starting", "instance_id", instanceID, "db", cfg.DBPath, "addr", cfg.Addr)

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			slog.Error("closing database", "error", err)
		}
	}()

	// Migrate on boot so a fresh checkout runs with no separate setup step.
	migrateCtx, cancelMigrate := context.WithTimeout(context.Background(), 30*time.Second)
	err = db.Migrate(migrateCtx)
	cancelMigrate()
	if err != nil {
		return err
	}
	slog.Info("migrations applied")

	clk := clock.Real{}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpapi.New(db, cfg, clk).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Signal handling is installed before the listener starts, so a Ctrl+C
	// during startup is not lost.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		slog.Info("shutdown signal received", "grace", cfg.ShutdownGrace)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown timed out; forcing close", "error", err)
		_ = srv.Close()
	}

	slog.Info("stopped")
	return nil
}

func setupLogging(cfg config.Config) {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var h slog.Handler
	if cfg.LogLevelIsJSON {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}
