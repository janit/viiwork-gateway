// Command viiwork-gateway exposes the viiwork mesh on one authenticated port.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/janit/viiwork-gateway/internal/accesslog"
	"github.com/janit/viiwork-gateway/internal/auth"
	"github.com/janit/viiwork-gateway/internal/config"
	"github.com/janit/viiwork-gateway/internal/fleet"
	"github.com/janit/viiwork-gateway/internal/keys"
	"github.com/janit/viiwork-gateway/internal/proxy"
	"github.com/janit/viiwork-gateway/internal/ratelimit"
)

// version is stamped at build time with -X main.version.
var version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load(os.Getenv, os.Hostname)
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

	fl, err := fleet.Start(ctx, fleet.Options{
		Config:  cfg,
		Version: version,
		Logf:    func(format string, args ...any) { logger.Info(fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		logger.Error("join mesh", "err", err)
		os.Exit(1)
	}

	// Listening is separated from serving so a bind failure is reported here,
	// where the mesh can still be left cleanly, rather than from inside serve
	// after the gateway has already announced itself to every member.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Error("listen", "addr", cfg.Listen, "err", err)
		_ = fl.Close(5 * time.Second)
		os.Exit(1)
	}

	srv := &http.Server{
		Handler: buildHandler(fl, set, cfg, logger),
		// Streams run for as long as a generation takes, so no write
		// deadline. Headers must still arrive promptly.
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
	}

	logger.Info("viiwork-gateway listening",
		"addr", ln.Addr().String(), "name", fl.Name(), "mode", fl.Mode(),
		"advertise", fl.Advertise().String(), "keys", set.Len(), "version", version)

	if err := serve(ctx, srv, ln, fl, logger); err != nil {
		logger.Error("gateway stopped", "err", err)
		os.Exit(1)
	}
	logger.Info("viiwork-gateway stopped")
}

// lifecycle is the part of fleet.Fleet the shutdown path uses. It is an
// interface so the ordering can be tested without a mesh.
type lifecycle interface {
	Close(leaveTimeout time.Duration) error
	Fatal() <-chan error
}

// serve runs the HTTP server until the context ends, the mesh reports a fatal
// error, or Serve itself fails — then shuts down in that order: drain HTTP,
// then leave the mesh.
//
// The order is the point. Draining still needs the mesh: a request that is
// mid-flight may yet be forwarded to a node, and leaving first would sever
// exactly the requests the drain exists to protect. Leaving afterwards makes
// the gateway disappear from every member at once, rather than being declared
// dead a failure-detector interval later.
func serve(ctx context.Context, srv *http.Server, ln net.Listener, fl lifecycle, logger *slog.Logger) error {
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	var stopErr error
	select {
	case <-ctx.Done():
	case err := <-fl.Fatal():
		// Another member already holds this name. There is no version of
		// this process that can keep running.
		logger.Error("fatal mesh error", "err", err)
		stopErr = err
	case err := <-serveErr:
		if err != nil {
			logger.Error("server", "err", err)
			stopErr = err
		}
	}

	// net/http's own docs warn against exiting as soon as Serve returns:
	// "Make sure the program doesn't exit and waits instead for Shutdown to
	// return." So the 15s drain deadline below is actually honoured.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "err", err)
	}

	if err := fl.Close(5 * time.Second); err != nil {
		logger.Error("leave mesh", "err", err)
	}
	return stopErr
}

// buildHandler composes the request pipeline. Order matters: authentication is
// outermost so nothing downstream ever sees an unauthenticated request, and the
// access log sits inside it so it can record the key's label.
func buildHandler(fl proxy.Fleet, set *keys.Set, cfg config.Config, logger *slog.Logger) http.Handler {
	// Per-key rate limiting lives inside the router rather than as its own
	// middleware, so that the decision about WHICH traffic is metered (the
	// model-routed path, not the dashboards) is made in the one place that
	// already classifies it. A middleware would have to re-derive that
	// classification and could drift out of step with the router's.
	limiter := ratelimit.New(cfg.RatePerMin, cfg.RateBurst)
	router := proxy.NewRouter(fl, proxy.NewForwarder(logger), cfg.MaxBody, cfg.MaxInFlight, cfg.BodyReadTimeout, limiter, logger)
	mw := &auth.Middleware{Keys: set, TTL: cfg.CookieTTL, Logger: logger}
	return mw.Wrap(accesslog.Wrap(router, logger))
}
