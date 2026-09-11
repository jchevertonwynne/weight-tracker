// Package serve runs an http.Server with the timeouts every app on the
// cluster needs and shuts it down on SIGTERM.
package serve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

const shutdownTimeout = 10 * time.Second

type config struct {
	logAttrs   []any
	onShutdown func()
}

type Option func(*config)

// WithLogAttrs adds attributes to the "listening" line, for whatever the app
// wants to record at startup (a database path, say). The address is always
// included.
func WithLogAttrs(attrs ...any) Option {
	return func(c *config) { c.logAttrs = append(c.logAttrs, attrs...) }
}

// WithPreShutdown runs f after the signal arrives but before Shutdown.
//
// This is for anything holding handlers open that Shutdown would otherwise
// wait for. An SSE stream is an ordinary handler as far as net/http is
// concerned, so Shutdown blocks until it returns — left running, every
// rollout burns the full timeout waiting for clients to disconnect on their
// own. Closing the hub here makes each stream's read loop see a closed
// channel and return, so Shutdown has nothing left to wait for.
func WithPreShutdown(f func()) Option {
	return func(c *config) { c.onShutdown = f }
}

// Run serves h on addr until SIGINT or SIGTERM, then drains in-flight
// requests. It returns a non-nil error if the listener could not be bound,
// which is worth exiting non-zero over: Kubernetes should restart a pod that
// cannot serve. Anything to flush on the way out goes after Run returns.
func Run(addr string, h http.Handler, opts ...Option) error {
	cfg := &config{}
	for _, o := range opts {
		o(cfg)
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: h,
		// Not theoretical on a public hostname: without ReadHeaderTimeout one
		// client holds a connection open indefinitely by dribbling out
		// headers, and enough of those exhaust a 4GB Pi without ever
		// completing a request.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", append([]any{"addr", addr}, cfg.logAttrs...)...)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("listen and serve: %w", err)
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	if cfg.onShutdown != nil {
		cfg.onShutdown()
	}

	// Kubernetes sends SIGTERM and waits terminationGracePeriodSeconds before
	// SIGKILL; this timeout is what keeps the drain inside that window.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
