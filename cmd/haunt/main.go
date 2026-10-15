package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/example-git/haunt-server/internal/config"
	"github.com/example-git/haunt-server/internal/httpapi"
	"github.com/example-git/haunt-server/internal/identity"
	"github.com/example-git/haunt-server/internal/platform/postgres"
	"github.com/example-git/haunt-server/internal/setup"
)

var version = "0.1.0"
var passwordFile string

func main() {
	showVersion := flag.Bool("version", false, "print version")
	flag.StringVar(&passwordFile, "owner-password-file", "", "reset owner password from this host-local file and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var cfg config.Config
	var host *setup.Host
	var err error
	if passwordFile != "" {
		values, _, loadErr := setup.Values()
		if loadErr != nil {
			return loadErr
		}
		cfg, err = config.LoadValues(values)
	} else {
		cfg, host, err = setup.Begin(ctx, logger)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		return err
	}
	if host != nil {
		defer host.Close()
	}
	startupCtx, cancelStartup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStartup()
	database, err := postgres.Open(startupCtx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer database.Close()
	if err := postgres.Migrate(startupCtx, database, cfg.InstanceID); err != nil {
		return fmt.Errorf("database migration: %w", err)
	}
	identities, err := identity.New(startupCtx, cfg, database)
	if err != nil {
		return fmt.Errorf("identity startup: %w", err)
	}
	if passwordFile != "" {
		file, err := os.Open(passwordFile)
		if err != nil {
			return fmt.Errorf("could not open owner password file")
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 1026))
		if err != nil || len(data) > 1025 {
			return fmt.Errorf("could not read a bounded owner password")
		}
		password := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
		if err := identities.ResetOwner(startupCtx, password); err != nil {
			return fmt.Errorf("owner password reset failed: %w", err)
		}
		logger.Info("owner password reset; all owner sessions revoked")
		return nil
	}
	if err := identities.Cleanup(startupCtx); err != nil {
		return fmt.Errorf("identity cleanup: %w", err)
	}
	features, err := httpapi.NewFeatures(startupCtx, cfg, database, identities, logger)
	if err != nil {
		return fmt.Errorf("feature startup: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := features.Close(closeCtx); err != nil {
			logger.Error("feature shutdown failed", "error", err)
		}
	}()
	cancelStartup()
	maintenanceCtx, cancelMaintenance := context.WithCancel(context.Background())
	maintenanceDone := make(chan struct{})
	go func() {
		defer close(maintenanceDone)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-maintenanceCtx.Done():
				return
			case <-ticker.C:
				cleanupCtx, cancel := context.WithTimeout(maintenanceCtx, 5*time.Second)
				err := identities.Cleanup(cleanupCtx)
				if err == nil {
					err = features.Maintain(cleanupCtx)
				}
				cancel()
				if err != nil && maintenanceCtx.Err() == nil {
					logger.Error("maintenance failed", "error", err)
				}
			}
		}
	}()
	defer func() { cancelMaintenance(); <-maintenanceDone }()

	server := httpapi.New(cfg, identities, logger, features)
	var listener net.Listener
	var serveErr chan error
	if host != nil {
		host.Use(server.Handler)
		server = host.Server
		listener = host.Listener
		serveErr = host.Errors
	} else {
		listener, err = net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		serveErr = make(chan error, 1)
		go func() { serveErr <- server.Serve(listener) }()
	}
	defer listener.Close()
	logger.Info("HTTP listener opened", "address", listener.Addr().String(), "instance_id", cfg.InstanceID)

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			_ = server.Close()
			return fmt.Errorf("HTTP server: %w", err)
		}
		return nil
	case <-ctx.Done():
		// Let a second interrupt force process termination during shutdown.
		stop()
		logger.Info("draining HTTP requests")
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelShutdown()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("HTTP shutdown: %w", err)
		}
		err := <-serveErr
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP server: %w", err)
		}
		return nil
	}
}
