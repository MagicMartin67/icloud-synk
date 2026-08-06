package synk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

// photoPageSize is how many assets are requested per CloudKit query.
const photoPageSize = 200

// SyncPhotos downloads everything from the iCloud photo library that is not
// recorded in the state yet.
//
// This direction is one-way by design: uploading into the photo library needs
// undocumented CloudKit asset-upload calls that can corrupt the library, so
// files placed in Photos/ by hand are left alone.
func (e *Engine) SyncPhotos(ctx context.Context) (Stats, error) {
	var stats Stats
	if err := os.MkdirAll(e.cfg.PhotosDir(), 0o755); err != nil {
		return stats, err
	}

	used := e.st.PhotoPaths()
	seen, known := 0, 0
	e.log.Info("photo pass started", "dir", e.cfg.PhotosDir())

	err := e.cl.ListPhotos(ctx, photoPageSize, func(p icloud.Photo) error {
		seen++
		if err := ctx.Err(); err != nil {
			return err
		}
		if prev, ok := e.st.Photo(p.ID); ok {
			// Already downloaded once. If the asset was replaced in iCloud its
			// checksum changes, otherwise nothing to do — including when the
			// user deleted the local copy on purpose.
			if prev.Checksum == p.Checksum || p.Checksum == "" {
				known++
				return nil
			}
		}

		rel := e.photoRelPath(p, used)
		dst := filepath.Join(e.cfg.PhotosDir(), filepath.FromSlash(rel))

		if e.dryRun {
			e.log.Info("would download photo", "path", rel, "bytes", p.Size)
			stats.Downloaded++
			used[rel] = p.ID
			return nil
		}

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		n, err := e.downloadPhoto(ctx, p, dst)
		if err != nil {
			e.log.Error("downloading photo", "path", rel, "err", err)
			stats.Errors++
			return nil // one bad asset must not stop the library
		}
		mtime := p.Created
		if mtime.IsZero() {
			mtime = time.Now()
		}
		if err := os.Chtimes(dst, mtime, mtime); err != nil {
			e.log.Debug("setting photo mtime", "path", rel, "err", err)
		}
		e.st.PutPhoto(p.ID, PhotoEntry{
			Path:     rel,
			Size:     n,
			Checksum: p.Checksum,
			Created:  p.Created,
		})
		used[rel] = p.ID
		stats.Downloaded++
		stats.Bytes += n
		e.log.Info("downloaded photo", "path", rel, "bytes", n)

		// Persist periodically so a crash does not re-download everything.
		if stats.Downloaded%50 == 0 {
			if err := e.st.Save(); err != nil {
				e.log.Warn("saving state", "err", err)
			}
		}
		return nil
	})
	if err != nil && !errorsIsContext(err) {
		return stats, fmt.Errorf("listing iCloud photos: %w", err)
	}
	if saveErr := e.st.Save(); saveErr != nil {
		e.log.Warn("saving state", "err", saveErr)
	}
	// Always report, including "nothing happened": a silent pass is
	// indistinguishable from a broken one.
	e.log.Info("photo pass finished",
		"assets_listed", seen, "already_had", known,
		"downloaded", stats.Downloaded, "errors", stats.Errors)
	if seen == 0 {
		e.log.Warn("iCloud reported no photos at all — if the library is not empty, " +
			"run `icloud-synk sync --photos --verbose` and check the response")
	}
	return stats, nil
}

// photoRelPath lays photos out as YYYY/MM/name, resolving name collisions
// between different assets with a short suffix from the record id.
func (e *Engine) photoRelPath(p icloud.Photo, used map[string]string) string {
	name := p.SafeFilename()
	dir := "unknown-date"
	if !p.Created.IsZero() {
		dir = p.Created.Format("2006/01")
	}
	rel := dir + "/" + name
	if owner, taken := used[rel]; !taken || owner == p.ID {
		return rel
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	suffix := p.ID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return dir + "/" + stem + "-" + suffix + ext
}

func (e *Engine) downloadPhoto(ctx context.Context, p icloud.Photo, dst string) (int64, error) {
	f, err := os.CreateTemp(filepath.Dir(dst), ".icloud-synk-*.part")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	defer func() {
		f.Close()
		os.Remove(tmp)
	}()
	n, err := e.cl.DownloadPhoto(ctx, p, f)
	if err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return 0, err
	}
	return n, nil
}

func errorsIsContext(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
