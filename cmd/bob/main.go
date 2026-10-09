// bob (Backend Of Backend) is the in-cluster app server for zengine.
// It serves features directly (/aigate) and forwards the rest to upstream services (proxies).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"bob/internal/aigate"
	"bob/internal/config"
	"bob/internal/logging"
	"bob/internal/proxy"
	"bob/internal/search"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to YAML config")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		fatal(slog.Default(), "failed to load config", err)
	}
	logger := logging.New(os.Stderr, cfg.Server.LogLevel)
	slog.SetDefault(logger)

	redactedCfg, err := config.RedactedYAML(cfg)
	if err != nil {
		fatal(logger, "failed to render startup config", err)
	}
	logger.Info("loaded config", "config", redactedCfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown route", "path": r.URL.Path})
	})

	if cfg.AIGate.Enabled() {
		gw, err := aigate.NewGateway(&cfg.AIGate, logger)
		if err != nil {
			fatal(logger, "failed to initialize aigate", err)
		}
		gw.LoadAvailableModels(context.Background())
		if *cfg.AIGate.ValidateModelsOnStartup {
			if err := gw.ValidateStartupModels(); err != nil {
				fatal(logger, "failed startup model validation", err)
			}
		}
		mux.Handle("/aigate/", http.StripPrefix("/aigate", gw.Handler()))
		logger.Info("route", "path", "/aigate/", "handler", "aigate")
	}

	if cfg.Search.Enabled() {
		engines := search.Engines(cfg.Search)
		engineNames := make([]string, len(engines))
		for i, e := range engines {
			engineNames[i] = e.Name
		}
		mux.Handle("/search/", http.StripPrefix("/search", search.NewHandler(engines, logger)))
		logger.Info("route", "path", "/search/", "handler", "search", "engines", engineNames)
	}

	names := make([]string, 0, len(cfg.Proxies))
	for name := range cfg.Proxies {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		h, err := proxy.New(name, cfg.Proxies[name], logger)
		if err != nil {
			fatal(logger, "failed to initialize proxy", err)
		}
		mux.Handle("/"+name+"/", h)
		logger.Info("route", "path", "/"+name+"/", "upstream", cfg.Proxies[name])
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:           logging.Middleware(logger, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("starting bob", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal(logger, "server error", err)
	}
}

func fatal(l *slog.Logger, msg string, err error) {
	l.Error(msg, "err", err)
	os.Exit(1)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
