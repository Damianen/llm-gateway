// Command gateway runs the llm-gateway server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Damianen/llm-gateway/internal/config"
	"github.com/Damianen/llm-gateway/internal/router"
	"github.com/Damianen/llm-gateway/internal/server"
	"github.com/Damianen/llm-gateway/internal/store"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "llm-gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to YAML configuration")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.Server.LogLevel)
	slog.SetDefault(logger)

	st, err := store.Open(cfg.Database.Path)
	if err != nil {
		return err
	}
	defer st.Close()

	adminToken := os.Getenv("GATEWAY_ADMIN_TOKEN")
	if adminToken == "" {
		logger.Warn("GATEWAY_ADMIN_TOKEN is not set: admin API is disabled")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 32
	upstreamClient := &http.Client{Transport: transport} // timeouts are per-request via context

	rt, err := router.New(cfg, upstreamClient)
	if err != nil {
		return err
	}

	srv := server.New(server.Options{
		Config:     cfg,
		Logger:     logger,
		Store:      st,
		Router:     rt,
		AdminToken: adminToken,
		Version:    version,
	})

	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// WriteTimeout stays 0: streaming responses are long-lived.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("llm-gateway listening", "addr", cfg.Server.Listen, "version", version, "db", cfg.Database.Path)
		serveErr <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down", "grace", cfg.Server.ShutdownGrace.Std().String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownGrace.Std())
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		httpSrv.Close()
		if !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("shutdown: %w", err)
		}
		logger.Warn("shutdown grace elapsed; connections closed forcefully")
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
