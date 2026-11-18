package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/wolfeidau/durable/internal/config"
	"github.com/wolfeidau/durable/internal/otlp"
	"github.com/wolfeidau/durable/internal/sandbox/openshell"
	"github.com/wolfeidau/durable/internal/telemetry"
)

const (
	otlpHeadersFile = "otlp-headers"
	telemetryDB     = "telemetry.db"
	pruneInterval   = time.Hour
)

var otlpHeadersFormat = regexp.MustCompile(`^authorization=Bearer ([0-9a-f]{64})\n$`)

// loadOrCreateOTLPHeaders keeps the headers file across restarts, because the gateway reads it only at startup.
func loadOrCreateOTLPHeaders(path string) (string, error) {
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	token := hex.EncodeToString(raw[:])
	err := writeFileAtomic(path, []byte("authorization=Bearer "+token+"\n"), false)
	if err == nil {
		return token, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return "", err
	}
	data, err := readPrivateFile(path)
	if err != nil {
		return "", err
	}
	m := otlpHeadersFormat.FindStringSubmatch(string(data))
	if m == nil {
		return "", fmt.Errorf("%s is not in the form authorization=Bearer <64 hex characters>; delete it to create a new one", path)
	}
	return m[1], nil
}

// receiver is the OTLP listener and the store behind it.
type receiver struct {
	addr string
	srv  *http.Server
	// errc reports a failed Serve; it also receives http.ErrServerClosed on shutdown.
	errc  chan error
	store *telemetry.Store

	stopPrune context.CancelFunc
	wg        sync.WaitGroup
	once      sync.Once
	closeErr  error
}

// startReceiver returns nil when telemetry is off; the receiver's methods accept nil.
// It validates and opens everything before listening, so a bad file or a port clash fails startup.
func startReceiver(ctx context.Context, logger *slog.Logger, home string, settings *config.Settings, baseContext func(net.Listener) context.Context) (*receiver, []string, error) {
	if settings.TelemetryAddr == "" {
		return nil, nil, nil
	}
	dir := filepath.Join(home, ".durable")
	headers := filepath.Join(dir, otlpHeadersFile)
	token, err := loadOrCreateOTLPHeaders(headers)
	if err != nil {
		return nil, nil, err
	}
	store, err := telemetry.Open(ctx, filepath.Join(dir, telemetryDB))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open telemetry store: %w", err)
	}
	handler, err := otlp.Handler(token, func(ctx context.Context, recs []otlp.Record) error {
		skipped, err := store.Add(ctx, events(recs, time.Now()))
		if skipped > 0 {
			logger.WarnContext(ctx, "telemetry dropped network decisions it could not match to a sandbox", slog.Int("count", skipped))
		}
		return err
	})
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("failed to create telemetry handler: %w", err)
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "tcp", settings.TelemetryAddr)
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("failed to listen on %s for telemetry: %w", settings.TelemetryAddr, err)
	}
	r := &receiver{
		addr:  ln.Addr().String(),
		store: store,
		errc:  make(chan error, 1),
		srv: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			BaseContext:       baseContext,
		},
	}
	var warnings []string
	if !isLoopback(ln.Addr()) {
		const msg = "telemetry receiver is listening on a non-loopback address: other machines can reach it over plain HTTP, guarded only by the bearer token"
		logger.Warn(msg, slog.String("addr", r.addr))
		warnings = append(warnings, msg+" ("+r.addr+")")
	}
	pruneCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	r.stopPrune = stop
	r.wg.Go(func() { r.errc <- r.srv.Serve(ln) })
	r.wg.Go(func() { prune(pruneCtx, logger, store, settings.TelemetryRetention) })
	logger.Info("telemetry receiver", slog.String("addr", r.addr), slog.String("headers_file", headers))
	return r, warnings, nil
}

// events converts records to store events; records that aren't network decisions are dropped.
func events(recs []otlp.Record, now time.Time) []telemetry.Event {
	var out []telemetry.Event
	for _, rec := range recs {
		observed := rec.Observed
		if observed.IsZero() {
			observed = now
		}
		if rec.Gap != nil {
			out = append(out, telemetry.Event{SandboxID: rec.SandboxID, Observed: observed, Gap: rec.Gap})
			continue
		}
		d, name, id, ok := openshell.ParseOCSF(rec.Body)
		if !ok {
			continue
		}
		if id == "" {
			id = rec.SandboxID
		}
		out = append(out, telemetry.Event{Decision: d, UID: rec.UID, SandboxName: name, SandboxID: id, Observed: observed, Body: rec.Body})
	}
	return out
}

// prune logs at warn because it runs on its own goroutine, with no caller to return the error to.
func prune(ctx context.Context, logger *slog.Logger, store *telemetry.Store, retention time.Duration) {
	tick := time.NewTicker(pruneInterval)
	defer tick.Stop()
	for {
		if _, err := store.Prune(ctx, time.Now().Add(-retention)); err != nil && ctx.Err() == nil {
			logger.WarnContext(ctx, "failed to prune telemetry", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (r *receiver) address() string {
	if r == nil {
		return ""
	}
	return r.addr
}

// failed never fires when r is nil, because a nil channel blocks.
func (r *receiver) failed() <-chan error {
	if r == nil {
		return nil
	}
	return r.errc
}

// close stops accepting, waits for in-flight exports and the prune loop, then closes the store; it is safe to call twice.
func (r *receiver) close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		r.stopPrune()
		err := r.srv.Shutdown(ctx)
		if err != nil {
			_ = r.srv.Close()
		}
		r.wg.Wait()
		if cerr := r.store.Close(); err == nil {
			err = cerr
		}
		r.closeErr = err
	})
	return r.closeErr
}

// readPrivateFile reads a file only its owner can access.
func readPrivateFile(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to stat %s: %w", path, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is accessible by other users (mode %04o); run chmod 600 %s", path, st.Mode().Perm(), path)
	}
	data, err := os.ReadFile(path) //nolint:gosec // fixed name inside the durable directory
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", path, err)
	}
	return data, nil
}
