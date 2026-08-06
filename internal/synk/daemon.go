package synk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/ak/icloud-synk/internal/icloud"
)

// Daemon keeps a folder and iCloud converged: local changes are picked up by
// inotify, remote changes by polling (Apple's web API has no push channel).
type Daemon struct {
	eng *Engine
	cl  *icloud.Client
	log *slog.Logger

	// reauth is set while the session is dead, so passes stop hammering the API.
	needsLogin atomic.Bool
}

// NewDaemon wires a daemon around an engine.
func NewDaemon(eng *Engine, cl *icloud.Client, log *slog.Logger) *Daemon {
	return &Daemon{eng: eng, cl: cl, log: log}
}

// Run blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	cfg := d.eng.cfg
	if err := d.eng.EnsureDirs(); err != nil {
		return err
	}

	trigger := make(chan struct{}, 1)
	poke := func() {
		select {
		case trigger <- struct{}{}:
		default: // a pass is already queued; passes are idempotent
		}
	}

	if cfg.SyncDrive {
		watcher, err := NewWatcher(cfg.DriveDir(), d.eng.scanner, d.log)
		if err != nil {
			return fmt.Errorf("watching %s: %w", cfg.DriveDir(), err)
		}
		defer watcher.Close()

		local := make(chan struct{}, 1)
		go watcher.Events(ctx, cfg.Debounce.D(), local)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-local:
					d.log.Debug("local change detected")
					poke()
				}
			}
		}()
	}

	poll := time.NewTicker(cfg.PollInterval.D())
	defer poll.Stop()

	photoTicker := &time.Ticker{C: nil}
	if cfg.SyncPhotos {
		photoTicker = time.NewTicker(cfg.PhotosInterval.D())
		defer photoTicker.Stop()
	}

	// Backoff after failures, so a network outage or a 429 does not turn into a
	// tight retry loop.
	backoff := time.Duration(0)
	const maxBackoff = 15 * time.Minute

	d.log.Info("daemon started",
		"target", cfg.TargetDir,
		"poll", cfg.PollInterval.D().String(),
		"photos", cfg.SyncPhotos,
		"workers", cfg.Workers)

	poke() // first pass immediately
	if cfg.SyncPhotos {
		go func() {
			// The first photo pass can take a long time; run it off the main loop
			// after a short delay so Drive syncs first.
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
				d.runPhotos(ctx)
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			d.log.Info("daemon stopping")
			return nil

		case <-poll.C:
			poke()

		case <-photoTicker.C:
			d.runPhotos(ctx)

		case <-trigger:
			if backoff > 0 {
				d.log.Debug("backing off", "wait", backoff.String())
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
			}
			if err := d.ensureSession(ctx); err != nil {
				backoff = nextBackoff(backoff, maxBackoff)
				continue
			}
			stats, err := d.eng.SyncDrive(ctx)
			switch {
			case err == nil:
				backoff = 0
				if !stats.Empty() {
					d.log.Info("drive pass", "result", stats.String())
				}
				d.eng.st.MarkSynced(time.Now())
			case errors.Is(err, context.Canceled):
				return nil
			default:
				backoff = nextBackoff(backoff, maxBackoff)
				d.log.Error("drive pass failed", "err", err, "retry_in", backoff.String())
				d.checkAuthError(err)
			}
		}
	}
}

func (d *Daemon) runPhotos(ctx context.Context) {
	if err := d.ensureSession(ctx); err != nil {
		return
	}
	stats, err := d.eng.SyncPhotos(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		d.log.Error("photo pass failed", "err", err)
		d.checkAuthError(err)
		return
	}
	if !stats.Empty() {
		d.log.Info("photo pass", "result", stats.String())
	}
}

// ensureSession re-validates the session, waiting for the user to run
// `icloud-synk login` when the trust token has expired.
func (d *Daemon) ensureSession(ctx context.Context) error {
	if d.needsLogin.Load() {
		// A fresh `icloud-synk login` writes a new session file; pick it up
		// without requiring a daemon restart.
		if err := d.cl.ReloadSession(); err != nil {
			d.log.Debug("reloading session", "err", err)
		}
	}
	err := d.cl.Authenticate(ctx)
	if err == nil {
		if d.needsLogin.Swap(false) {
			d.log.Info("session restored")
		}
		return nil
	}
	if errors.Is(err, icloud.ErrLoginRequired) {
		if !d.needsLogin.Swap(true) {
			d.log.Error("iCloud session expired — run `icloud-synk login` to sign in again")
			d.notify("iCloud sync paused",
				"The iCloud session has expired. Run `icloud-synk login` in a terminal.")
		}
		return err
	}
	d.log.Warn("could not verify the iCloud session", "err", err)
	return err
}

// checkAuthError promotes an auth failure seen mid-pass into the re-login state.
func (d *Daemon) checkAuthError(err error) {
	if errors.Is(err, icloud.ErrLoginRequired) && !d.needsLogin.Swap(true) {
		d.log.Error("iCloud session expired — run `icloud-synk login` to sign in again")
		d.notify("iCloud sync paused",
			"The iCloud session has expired. Run `icloud-synk login` in a terminal.")
	}
}

// notify shows a desktop notification; failures are not worth reporting twice.
func (d *Daemon) notify(title, body string) {
	if !d.eng.cfg.Notify {
		return
	}
	cmd := exec.Command("notify-send", "--app-name=icloud-synk", "--icon=folder-remote", title, body)
	if err := cmd.Run(); err != nil {
		d.log.Debug("notify-send failed", "err", err)
	}
}

// nextBackoff doubles the delay, starting at 15s and capping at max.
func nextBackoff(cur, max time.Duration) time.Duration {
	if cur <= 0 {
		return 15 * time.Second
	}
	cur *= 2
	if cur > max {
		return max
	}
	return cur
}
