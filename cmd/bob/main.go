// bob (Backend Of Backend) is the in-cluster app server for zengine.
// It serves features directly (/aigate, /runbox, ...) and forwards the rest to upstream services (proxies).
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
	"bob/internal/cloudflare"
	"bob/internal/config"
	"bob/internal/google"
	"bob/internal/logging"
	"bob/internal/metrics"
	"bob/internal/proxy"
	"bob/internal/runbox"
	"bob/internal/search"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to YAML config")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: bob [-config file] [pull-images]\n\n"+
			"Without a command, bob serves HTTP. pull-images pulls the runbox images the Docker host does not\n"+
			"have yet and exits (run it in an init container before bob serves /runbox/).\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 1 || (flag.NArg() == 1 && flag.Arg(0) != "pull-images") {
		flag.Usage()
		os.Exit(2)
	}

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

	if flag.Arg(0) == "pull-images" {
		pullImages(cfg, logger)
		return
	}

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

	if cfg.Metrics.Enabled() {
		metricNames := make([]string, 0, len(cfg.Metrics.Queries))
		for name := range cfg.Metrics.Queries {
			metricNames = append(metricNames, name)
		}
		slices.Sort(metricNames)
		mux.Handle("/metrics/", http.StripPrefix("/metrics", metrics.NewHandler(cfg.Metrics, logger)))
		logger.Info("route", "path", "/metrics/", "handler", "metrics", "prometheus", cfg.Metrics.Prometheus, "metrics", metricNames)
	}

	if cfg.Cloudflare.Enabled() {
		mux.Handle("/cloudflare/", http.StripPrefix("/cloudflare", cloudflare.NewHandler(cfg.Cloudflare, logger)))
		logger.Info("route", "path", "/cloudflare/", "handler", "cloudflare")
	}

	if cfg.Google.Enabled() {
		g, err := google.NewHandler(cfg.Google, logger)
		if err != nil {
			fatal(logger, "failed to initialize google", err)
		}
		if cfg.Google.GAPropertyID != "" {
			mux.Handle("/ga/", http.StripPrefix("/ga", g.GA()))
			logger.Info("route", "path", "/ga/", "handler", "google analytics", "property", cfg.Google.GAPropertyID)
		}
		if cfg.Google.GSCSiteURL != "" {
			mux.Handle("/gsc/", http.StripPrefix("/gsc", g.GSC()))
			logger.Info("route", "path", "/gsc/", "handler", "search console", "site", cfg.Google.GSCSiteURL)
		}
	}

	if cfg.Runbox.Enabled() {
		rb, err := runbox.NewHandler(cfg.Runbox, logger)
		if err != nil {
			fatal(logger, "failed to initialize runbox", err)
		}
		mux.Handle("/runbox/", http.StripPrefix("/runbox", rb))
		logger.Info("route", "path", "/runbox/", "handler", "runbox", "docker", cfg.Runbox.DockerHost, "runcontainers", runbox.RuncontainersVersion())
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

// pullImages pulls the runbox images. Failures are logged but do not fail the command: in an init container
// they would keep bob, and its other features, from starting; an image not pulled is pulled on first use.
func pullImages(cfg *config.Config, logger *slog.Logger) {
	if !cfg.Runbox.Enabled() {
		logger.Info("runbox is disabled: no images to pull")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	start := time.Now()
	logger.Info("pulling runbox images", "runcontainers", runbox.RuncontainersVersion(), "images", len(runbox.Images()))
	if err := runbox.PullImages(ctx, cfg.Runbox, logger); err != nil {
		logger.Error("some runbox images were not pulled; they are pulled on first use", "err", err)
	}
	logger.Info("pulled runbox images", "seconds", int(time.Since(start).Seconds()))
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
