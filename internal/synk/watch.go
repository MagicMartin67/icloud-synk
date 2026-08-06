package synk

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher reports local changes below a root. inotify is not recursive, so new
// directories are added to the watch list as they appear.
type Watcher struct {
	root    string
	scanner *Scanner
	log     *slog.Logger
	w       *fsnotify.Watcher
}

// NewWatcher starts watching root recursively.
func NewWatcher(root string, scanner *Scanner, log *slog.Logger) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{root: root, scanner: scanner, log: log, w: fw}
	if err := w.addTree(root); err != nil {
		fw.Close()
		return nil, err
	}
	return w, nil
}

// Close stops the watcher.
func (w *Watcher) Close() error { return w.w.Close() }

// addTree watches dir and every directory below it.
func (w *Watcher) addTree(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable directories are simply not watched
		}
		if !d.IsDir() {
			return nil
		}
		if p != w.root {
			rel := relSlash(w.root, p)
			if w.scanner.IsExcluded(rel) {
				return filepath.SkipDir
			}
		}
		if err := w.w.Add(p); err != nil {
			w.log.Debug("cannot watch directory", "path", p, "err", err)
		}
		return nil
	})
}

// Events delivers a signal on out whenever the tree changed and then stayed
// quiet for debounce. Coalescing matters: a single file copy produces dozens of
// inotify events, and every one of them would otherwise start a sync pass.
func (w *Watcher) Events(ctx context.Context, debounce time.Duration, out chan<- struct{}) {
	var timer *time.Timer
	var timerC <-chan time.Time

	arm := func() {
		if timer == nil {
			timer = time.NewTimer(debounce)
		} else {
			timer.Reset(debounce)
		}
		timerC = timer.C
	}

	for {
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return

		case ev, ok := <-w.w.Events:
			if !ok {
				return
			}
			rel := relSlash(w.root, ev.Name)
			if rel != "." && w.scanner.IsExcluded(rel) {
				continue
			}
			// Keep the watch list in step with the tree.
			if ev.Has(fsnotify.Create) {
				if fi, err := os.Lstat(ev.Name); err == nil && fi.IsDir() {
					if err := w.addTree(ev.Name); err != nil {
						w.log.Debug("watching new directory", "path", ev.Name, "err", err)
					}
				}
			}
			if ev.Has(fsnotify.Chmod) && !ev.Has(fsnotify.Write) {
				continue // metadata-only change: not worth a pass
			}
			w.log.Debug("local change", "path", rel, "op", ev.Op.String())
			arm()

		case err, ok := <-w.w.Errors:
			if !ok {
				return
			}
			w.log.Warn("watcher error", "err", err)

		case <-timerC:
			timerC = nil
			select {
			case out <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}
}
