// Command viiwork-gateway exposes the viiwork mesh on one authenticated port.
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

	"github.com/janit/viiwork-gateway/internal/accesslog"
	"github.com/janit/viiwork-gateway/internal/auth"
	"github.com/janit/viiwork-gateway/internal/config"
	"github.com/janit/viiwork-gateway/internal/keys"
	"github.com/janit/viiwork-gateway/internal/mesh"
	"github.com/janit/viiwork-gateway/internal/proxy"
	"github.com/janit/viiwork-gateway/internal/ratelimit"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		logger.Error("configuration", "err", err)
		os.Exit(1)
	}
	set, err := keys.Load(os.Environ())
	if err != nil {
		logger.Error("api keys", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := mesh.New(mesh.Options{
		Seeds:             cfg.Seeds,
		Interval:          cfg.PollInterval,
		Timeout:           cfg.PollTimeout,
		ViewPrefer:        cfg.ViewPrefer,
		AllowPrivatePeers: cfg.AllowPrivatePeers,
		Logger:            logger,
	})
	go reg.Run(ctx)

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: buildHandler(reg, set, cfg, logger),
		// Streams run for as long as a generation takes, so no write
		// deadline. Headers must still arrive promptly.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	// net/http's own docs warn against exiting main as soon as
	// ListenAndServe returns: "Make sure the program doesn't exit and waits
	// instead for Shutdown to return." shutdownDone is what main waits on, so
	// the 15s drain deadline below is actually honoured instead of racing the
	// process exit.
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown", "err", err)
		}
		close(shutdownDone)
	}()

	logger.Info("viiwork-gateway listening",
		"addr", cfg.Listen, "seeds", cfg.Seeds, "keys", set.Len())

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server", "err", err)
		os.Exit(1)
	}

	<-shutdownDone
	logger.Info("viiwork-gateway stopped")
}

// buildHandler composes the request pipeline. Order matters: authentication is
// outermost so nothing downstream ever sees an unauthenticated request, and the
// access log sits inside it so it can record the key's label.
func buildHandler(reg *mesh.Registry, set *keys.Set, cfg config.Config, logger *slog.Logger) http.Handler {
	// Per-key rate limiting lives inside the router rather than as its own
	// middleware, so that the decision about WHICH traffic is metered (the
	// model-routed path, not the dashboards) is made in the one place that
	// already classifies it. A middleware would have to re-derive that
	// classification and could drift out of step with the router's.
	limiter := ratelimit.New(cfg.RatePerMin, cfg.RateBurst)
	router := proxy.NewRouter(reg, proxy.NewForwarder(logger), cfg.MaxBody, cfg.MaxInFlight, cfg.BodyReadTimeout, limiter, logger)
	mw := &auth.Middleware{Keys: set, TTL: cfg.CookieTTL, Logger: logger}
	return mw.Wrap(accesslog.Wrap(router, logger))
}
