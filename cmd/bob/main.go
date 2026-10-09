// bob (Backend Of Backend) is the in-cluster app server for zengine.
// It serves features directly (/aigate) and forwards the rest to upstream services (proxies).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"bob/internal/aigate"
	"bob/internal/config"
	"bob/internal/logging"
	"bob/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to YAML config")
	flag.Parse()

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	redactedCfg, err := config.RedactedYAML(cfg)
	if err != nil {
		log.Fatalf("failed to render startup config: %v", err)
	}
	log.Printf("loaded config:\n%s", redactedCfg)

	logger := logging.New(cfg.Server.LogLevel)
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
			log.Fatalf("failed to initialize aigate: %v", err)
		}
		gw.LoadAvailableModels(context.Background())
		if *cfg.AIGate.ValidateModelsOnStartup {
			if err := gw.ValidateStartupModels(); err != nil {
				log.Fatalf("failed startup model validation: %v", err)
			}
		}
		mux.Handle("/aigate/", http.StripPrefix("/aigate", gw.Handler()))
		log.Printf("route /aigate/ -> aigate")
	}

	names := make([]string, 0, len(cfg.Proxies))
	for name := range cfg.Proxies {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		h, err := proxy.New(name, cfg.Proxies[name], logger)
		if err != nil {
			log.Fatalf("failed to initialize proxy: %v", err)
		}
		mux.Handle("/"+name+"/", h)
		log.Printf("route /%s/ -> %s", name, cfg.Proxies[name])
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

	log.Printf("starting bob on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
