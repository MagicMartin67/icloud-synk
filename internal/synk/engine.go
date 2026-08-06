package synk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ak/icloud-synk/internal/config"
	"github.com/ak/icloud-synk/internal/icloud"
)

// folderBatch is how many folders are requested per API call while walking.
const folderBatch = 30

// Engine performs synchronisation passes.
type Engine struct {
	cfg *config.Config
	cl  *icloud.Client
	st  *State
	log *slog.Logger

	scanner *Scanner
	dryRun  bool

	// remoteDirs maps a relative path (""=root) to its folder item, and is
	// rebuilt on every pass. Uploads need the parent's docwsid, and workers
	// may create folders concurrently, hence the mutex.
	dirMu      sync.Mutex
	remoteDirs map[string]icloud.Item

	// stale holds folders that were uploaded into and whose etags must be
	// re-read before the pass ends.
	staleMu sync.Mutex
	stale   map[string]struct{}
}

// NewEngine wires an engine. The client must already be authenticated.
func NewEngine(cfg *config.Config, cl *icloud.Client, st *State, log *slog.Logger, dryRun bool) *Engine {
	return &Engine{
		cfg: cfg, cl: cl, st: st, log: log, dryRun: dryRun,
		scanner: &Scanner{Root: cfg.DriveDir(), Exclude: cfg.Exclude, MaxSize: cfg.MaxFileSize},
	}
}

// Stats summarises a pass.
type Stats struct {
	Downloaded int
	Uploaded   int
	Created    int
	TrashedLoc int
	TrashedRem int
	Conflicts  int
	Skipped    int
	Errors     int
	Bytes      int64
}

func (s Stats) String() string {
	return fmt.Sprintf("↓%d ↑%d +%d 🗑%d/%d ⚡%d ⚠%d",
		s.Downloaded, s.Uploaded, s.Created, s.TrashedLoc, s.TrashedRem, s.Conflicts, s.Errors)
}

// Empty reports whether the pass changed nothing.
func (s Stats) Empty() bool {
	return s.Downloaded+s.Uploaded+s.Created+s.TrashedLoc+s.TrashedRem+s.Errors == 0
}

// EnsureDirs creates the local layout.
func (e *Engine) EnsureDirs() error {
	for _, d := range []string{e.cfg.DriveDir(), e.cfg.MetaDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if e.cfg.SyncPhotos {
		if err := os.MkdirAll(e.cfg.PhotosDir(), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// SyncDrive runs one full Drive pass: scan, list, reconcile, apply.
func (e *Engine) SyncDrive(ctx context.Context) (Stats, error) {
	var stats Stats
	if err := e.EnsureDirs(); err != nil {
		return stats, err
	}

	local, skipped, err := e.scanner.Scan()
	if err != nil {
		return stats, fmt.Errorf("scanning %s: %w", e.cfg.DriveDir(), err)
	}
	for _, s := range skipped {
		e.log.Debug("skipping local entry", "path", s)
	}

	remoteFiles, remoteDirs, err := e.fetchRemote(ctx)
	if err != nil {
		return stats, err
	}
	e.remoteDirs = remoteDirs

	remote := make(map[string]icloud.Item, len(remoteFiles)+len(remoteDirs))
	for k, v := range remoteFiles {
		remote[k] = v
	}
	for k, v := range remoteDirs {
		if k == "" {
			continue // the root itself is not an entry
		}
		remote[k] = v
	}

	e.log.Debug("drive pass scanned",
		"local", len(local), "remote_files", len(remoteFiles), "remote_dirs", len(remoteDirs))

	ops := Reconcile(local, remote, e.st.Drive(), Policy{
		DeleteLocal:  e.cfg.DeleteLocal,
		DeleteRemote: e.cfg.DeleteRemote,
		Conflict:     e.cfg.Conflict,
	})

	e.log.Debug("drive pass planned", "operations", len(ops))
	stats = e.apply(ctx, ops)
	if err := e.st.Save(); err != nil {
		e.log.Warn("saving state", "err", err)
	}
	return stats, nil
}

// fetchRemote walks the whole Drive hierarchy breadth-first, batching folder
// requests. Apple offers no change feed, so this is the polling mechanism.
func (e *Engine) fetchRemote(ctx context.Context) (files, dirs map[string]icloud.Item, err error) {
	files = map[string]icloud.Item{}
	dirs = map[string]icloud.Item{}

	// pending maps a drivewsid awaiting expansion to its relative path.
	pending := map[string]string{icloud.RootFolderID: ""}

	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		ids := make([]string, 0, len(pending))
		for id := range pending {
			ids = append(ids, id)
			if len(ids) == folderBatch {
				break
			}
		}
		sort.Strings(ids)

		folders, err := e.cl.ListFolders(ctx, ids)
		if err != nil {
			return nil, nil, fmt.Errorf("listing iCloud Drive folders: %w", err)
		}
		next := map[string]string{}
		for _, f := range folders {
			parentRel, ok := pending[f.DrivewsID]
			if !ok {
				continue
			}
			delete(pending, f.DrivewsID)
			dirs[parentRel] = f.Item

			for _, it := range f.Items {
				name := sanitizeName(it.FullName())
				if name == "" {
					continue
				}
				rel := name
				if parentRel != "" {
					rel = parentRel + "/" + name
				}
				if e.scanner.IsExcluded(rel) {
					continue
				}
				if it.IsDir() {
					dirs[rel] = it
					next[it.DrivewsID] = rel
					continue
				}
				files[rel] = it
			}
		}
		// Folders Apple did not return (deleted mid-walk) must not loop forever.
		for _, id := range ids {
			delete(pending, id)
		}
		for id, rel := range next {
			pending[id] = rel
		}
	}
	return files, dirs, nil
}

// sanitizeName makes an iCloud name usable as a single path segment.
func sanitizeName(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.TrimSpace(name)
	if name == "." || name == ".." {
		return ""
	}
	return name
}

// apply executes the operations: directories first (sequentially, so parents
// exist), then transfers in parallel, then deletions.
func (e *Engine) apply(ctx context.Context, ops []Op) Stats {
	var stats Stats
	passStamp := time.Now().Format("2006-01-02T15-04-05")

	var transfers []Op
	var trashRemote []Op
	var trashLocal []Op

	for _, op := range ops {
		if err := ctx.Err(); err != nil {
			return stats
		}
		switch op.Kind {
		case OpMkdirLocal:
			if e.dryRun {
				e.log.Info("would create local directory", "path", op.Path)
				stats.Created++
				continue
			}
			if err := os.MkdirAll(e.localPath(op.Path), 0o755); err != nil {
				e.log.Error("creating local directory", "path", op.Path, "err", err)
				stats.Errors++
				continue
			}
			e.recordDir(op.Path, op.Remote)
			stats.Created++

		case OpMkdirRemote:
			if e.dryRun {
				e.log.Info("would create iCloud folder", "path", op.Path)
				stats.Created++
				continue
			}
			if err := e.mkdirRemote(ctx, op.Path); err != nil {
				e.log.Error("creating iCloud folder", "path", op.Path, "err", err)
				stats.Errors++
				continue
			}
			stats.Created++

		case OpDownload, OpUpload:
			transfers = append(transfers, op)

		case OpRecord:
			e.recordSynced(op)

		case OpTrashLocal:
			trashLocal = append(trashLocal, op)

		case OpTrashRemote:
			trashRemote = append(trashRemote, op)

		case OpSkip:
			e.log.Warn("skipping", "path", op.Path, "reason", op.Note)
			stats.Skipped++

		case OpForget:
			e.st.DeleteDrive(op.Path)
		}
	}

	stats.add(e.runTransfers(ctx, transfers, passStamp))

	for _, op := range trashLocal {
		if e.dryRun {
			e.log.Info("would move to local trash", "path", op.Path, "reason", op.Note)
			stats.TrashedLoc++
			continue
		}
		if err := e.moveToLocalTrash(op.Path, passStamp); err != nil {
			e.log.Error("moving to local trash", "path", op.Path, "err", err)
			stats.Errors++
			continue
		}
		e.log.Info("moved to local trash", "path", op.Path, "reason", op.Note)
		e.st.DeleteDrive(op.Path)
		stats.TrashedLoc++
	}

	trashed, trashErrs := e.trashRemote(ctx, trashRemote)
	stats.TrashedRem += trashed
	stats.Errors += trashErrs

	if err := e.refreshEtags(ctx); err != nil {
		e.log.Debug("refreshing etags", "err", err)
	}
	return stats
}

func (s *Stats) add(o Stats) {
	s.Downloaded += o.Downloaded
	s.Uploaded += o.Uploaded
	s.Conflicts += o.Conflicts
	s.Errors += o.Errors
	s.Bytes += o.Bytes
	s.Skipped += o.Skipped
}

// runTransfers downloads and uploads with a bounded worker pool.
func (e *Engine) runTransfers(ctx context.Context, ops []Op, passStamp string) Stats {
	var (
		stats Stats
		mu    sync.Mutex
	)
	if len(ops) == 0 {
		return stats
	}
	workers := e.cfg.Workers
	if workers > len(ops) {
		workers = len(ops)
	}
	jobs := make(chan Op)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for op := range jobs {
				if ctx.Err() != nil {
					return
				}
				res := e.transfer(ctx, op, passStamp)
				mu.Lock()
				stats.add(res)
				mu.Unlock()
			}
		}()
	}
	for _, op := range ops {
		if ctx.Err() != nil {
			break
		}
		jobs <- op
	}
	close(jobs)
	wg.Wait()
	return stats
}

func (e *Engine) transfer(ctx context.Context, op Op, passStamp string) Stats {
	var stats Stats
	if op.Conflict {
		stats.Conflicts++
		e.log.Warn("conflict", "path", op.Path, "resolution", op.Note)
	}
	if e.dryRun {
		e.log.Info("would "+string(op.Kind), "path", op.Path, "note", op.Note)
		if op.Kind == OpDownload {
			stats.Downloaded++
		} else {
			stats.Uploaded++
		}
		return stats
	}

	var err error
	switch op.Kind {
	case OpDownload:
		var n int64
		n, err = e.download(ctx, op, passStamp)
		if err == nil {
			stats.Downloaded++
			stats.Bytes += n
			e.log.Info("downloaded", "path", op.Path, "bytes", n)
		}
	case OpUpload:
		var n int64
		n, err = e.upload(ctx, op, passStamp)
		switch {
		case errors.Is(err, errUnchanged):
			err = nil
		case err == nil:
			stats.Uploaded++
			stats.Bytes += n
			e.log.Info("uploaded", "path", op.Path, "bytes", n)
		}
	}
	if err != nil {
		e.log.Error(string(op.Kind)+" failed", "path", op.Path, "err", err)
		stats.Errors++
	}
	return stats
}

// errUnchanged means an upload was skipped because the contents already match.
var errUnchanged = errors.New("contents unchanged")

func (e *Engine) localPath(rel string) string {
	return filepath.Join(e.cfg.DriveDir(), filepath.FromSlash(rel))
}

// download writes the remote file atomically and records the result.
func (e *Engine) download(ctx context.Context, op Op, passStamp string) (int64, error) {
	dst := e.localPath(op.Path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}

	// Preserve the local version when it is the conflict loser.
	if op.Conflict && op.Winner == WinnerRemote {
		switch e.cfg.Conflict {
		case "both":
			cp := conflictPath(dst, passStamp)
			if err := os.Rename(dst, cp); err != nil && !os.IsNotExist(err) {
				return 0, fmt.Errorf("saving conflicting local copy: %w", err)
			}
			e.log.Warn("kept local version alongside", "path", op.Path, "as", filepath.Base(cp))
		case "remote":
			if err := e.moveToLocalTrash(op.Path, passStamp); err != nil && !os.IsNotExist(err) {
				return 0, fmt.Errorf("moving local version to trash: %w", err)
			}
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".icloud-synk-*.part")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	h := sha256.New()
	n, err := e.cl.Download(ctx, op.Remote, io.MultiWriter(tmp, h))
	if err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return 0, err
	}

	mtime := op.Remote.ModTime()
	if mtime.IsZero() {
		mtime = time.Now()
	}
	if err := os.Chtimes(dst, mtime, mtime); err != nil {
		e.log.Debug("setting mtime", "path", op.Path, "err", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		return n, err
	}
	e.st.PutDrive(op.Path, Entry{
		Size:        info.Size(),
		LocalMTime:  info.ModTime(),
		SHA256:      hex.EncodeToString(h.Sum(nil)),
		Etag:        op.Remote.Etag,
		DrivewsID:   op.Remote.DrivewsID,
		DocwsID:     op.Remote.DocwsID,
		Zone:        op.Remote.Zone,
		RemoteMTime: op.Remote.ModTime(),
	})
	return n, nil
}

// upload sends a local file to iCloud, creating parent folders as needed.
func (e *Engine) upload(ctx context.Context, op Op, passStamp string) (int64, error) {
	src := e.localPath(op.Path)
	info, err := os.Stat(src)
	if err != nil {
		return 0, err
	}

	// Skip files that only got their timestamp touched.
	if prev, ok := e.st.Drive()[op.Path]; ok && prev.SHA256 != "" && prev.Size == info.Size() {
		sum, hashErr := fileSHA256(src)
		if hashErr == nil && sum == prev.SHA256 {
			prev.LocalMTime = info.ModTime()
			e.st.PutDrive(op.Path, prev)
			return 0, errUnchanged
		}
	}

	// The remote version loses a conflict: keep a copy next to the local file.
	if op.Conflict && op.Winner == WinnerLocal && e.cfg.Conflict == "both" && op.Remote.DocwsID != "" {
		cp := conflictPath(src, passStamp)
		if err := e.downloadTo(ctx, op.Remote, cp); err != nil {
			e.log.Warn("could not save the iCloud version of a conflict",
				"path", op.Path, "err", err)
		} else {
			e.log.Warn("kept iCloud version alongside", "path", op.Path, "as", filepath.Base(cp))
		}
	}

	parentRel := path.Dir(op.Path)
	if parentRel == "." {
		parentRel = ""
	}
	parent, err := e.ensureRemoteDir(ctx, parentRel)
	if err != nil {
		return 0, err
	}

	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	h := sha256.New()
	name := path.Base(op.Path)
	if err := e.cl.Upload(ctx, parent.Zone, parent.DocwsID, name, info.Size(), info.ModTime(),
		io.TeeReader(f, h)); err != nil {
		return 0, err
	}

	// The new etag is unknown until the folder is re-listed; refreshEtags does
	// that once per pass. Keep the previous ids so the entry stays usable.
	prev := e.st.Drive()[op.Path]
	e.st.PutDrive(op.Path, Entry{
		Size:        info.Size(),
		LocalMTime:  info.ModTime(),
		SHA256:      hex.EncodeToString(h.Sum(nil)),
		Etag:        "", // refreshed below
		DrivewsID:   prev.DrivewsID,
		DocwsID:     prev.DocwsID,
		Zone:        parent.Zone,
		RemoteMTime: info.ModTime(),
	})
	e.markStale(parentRel)
	return info.Size(), nil
}

// downloadTo fetches a remote item into an explicit local path.
func (e *Engine) downloadTo(ctx context.Context, item icloud.Item, dst string) error {
	f, err := os.CreateTemp(filepath.Dir(dst), ".icloud-synk-*.part")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer func() {
		f.Close()
		os.Remove(tmpName)
	}()
	if _, err := e.cl.Download(ctx, item, f); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// mkdirRemote creates a folder (and any missing ancestors) in iCloud.
func (e *Engine) mkdirRemote(ctx context.Context, rel string) error {
	_, err := e.ensureRemoteDir(ctx, rel)
	return err
}

// ensureRemoteDir returns the folder item for rel, creating it if needed.
func (e *Engine) ensureRemoteDir(ctx context.Context, rel string) (icloud.Item, error) {
	if rel == "" || rel == "." {
		root, ok := e.lookupDir("")
		if !ok {
			return icloud.Item{}, errors.New("iCloud Drive root is unknown")
		}
		return root, nil
	}
	if it, ok := e.lookupDir(rel); ok {
		return it, nil
	}
	parent, err := e.ensureRemoteDir(ctx, path.Dir(rel))
	if err != nil {
		return icloud.Item{}, err
	}
	name := path.Base(rel)
	created, err := e.cl.CreateFolder(ctx, parent.DrivewsID, name)
	if err != nil {
		return icloud.Item{}, err
	}
	if created.Zone == "" {
		created.Zone = parent.Zone
	}
	e.putDir(rel, created)
	e.recordDir(rel, created)
	e.log.Info("created iCloud folder", "path", rel)
	return created, nil
}

func (e *Engine) lookupDir(rel string) (icloud.Item, bool) {
	e.dirMu.Lock()
	defer e.dirMu.Unlock()
	it, ok := e.remoteDirs[rel]
	return it, ok
}

func (e *Engine) putDir(rel string, item icloud.Item) {
	e.dirMu.Lock()
	defer e.dirMu.Unlock()
	if e.remoteDirs == nil {
		e.remoteDirs = map[string]icloud.Item{}
	}
	e.remoteDirs[rel] = item
}

func (e *Engine) recordDir(rel string, item icloud.Item) {
	e.st.PutDrive(rel, Entry{
		IsDir:       true,
		Etag:        item.Etag,
		DrivewsID:   item.DrivewsID,
		DocwsID:     item.DocwsID,
		Zone:        item.Zone,
		RemoteMTime: item.ModTime(),
		LocalMTime:  time.Now(),
	})
}

// recordSynced stores the identifiers of a path that needs no transfer.
func (e *Engine) recordSynced(op Op) {
	prev := e.st.Drive()[op.Path]
	entry := Entry{
		IsDir:       op.Remote.IsDir(),
		Size:        op.Local.Size,
		LocalMTime:  op.Local.MTime,
		SHA256:      prev.SHA256,
		Etag:        op.Remote.Etag,
		DrivewsID:   op.Remote.DrivewsID,
		DocwsID:     op.Remote.DocwsID,
		Zone:        op.Remote.Zone,
		RemoteMTime: op.Remote.ModTime(),
	}
	if entry.IsDir {
		entry.Size = 0
	}
	e.st.PutDrive(op.Path, entry)
}

// trashRemote moves items to the iCloud trash in batches.
func (e *Engine) trashRemote(ctx context.Context, ops []Op) (done, errs int) {
	for len(ops) > 0 {
		n := min(len(ops), folderBatch)
		batch := ops[:n]
		ops = ops[n:]

		items := make([]icloud.Item, 0, len(batch))
		for _, op := range batch {
			items = append(items, op.Remote)
		}
		if e.dryRun {
			for _, op := range batch {
				e.log.Info("would move to iCloud trash", "path", op.Path, "reason", op.Note)
			}
			done += len(batch)
			continue
		}
		if err := e.cl.MoveToTrash(ctx, items); err != nil {
			e.log.Error("moving to iCloud trash", "err", err, "count", len(batch))
			errs += len(batch)
			continue
		}
		for _, op := range batch {
			e.log.Info("moved to iCloud trash", "path", op.Path, "reason", op.Note)
			e.st.DeleteDrive(op.Path)
		}
		done += len(batch)
	}
	return done, errs
}

// moveToLocalTrash relocates a local path under .icloud-synk/trash/<stamp>/.
func (e *Engine) moveToLocalTrash(rel, stamp string) error {
	src := e.localPath(rel)
	dst := filepath.Join(e.cfg.TrashDir(), stamp, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		// A cross-device rename cannot happen inside one tree, but a busy file
		// can still fail; fall back to copy+remove.
		if copyErr := copyFile(src, dst); copyErr != nil {
			return err
		}
		return os.RemoveAll(src)
	}
	return nil
}

// markStale notes a folder whose etags must be re-read after uploads.
func (e *Engine) markStale(rel string) {
	e.staleMu.Lock()
	if e.stale == nil {
		e.stale = map[string]struct{}{}
	}
	e.stale[rel] = struct{}{}
	e.staleMu.Unlock()
}

// refreshEtags re-lists the folders we uploaded into, so the state holds the
// etag iCloud assigned. Without it, the next pass would see a phantom change.
func (e *Engine) refreshEtags(ctx context.Context) error {
	e.staleMu.Lock()
	stale := e.stale
	e.stale = nil
	e.staleMu.Unlock()

	if len(stale) == 0 || e.dryRun {
		return nil
	}
	byID := make(map[string]string, len(stale))
	for rel := range stale {
		if it, ok := e.lookupDir(rel); ok {
			byID[it.DrivewsID] = rel
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	for len(ids) > 0 {
		n := min(len(ids), folderBatch)
		folders, err := e.cl.ListFolders(ctx, ids[:n])
		if err != nil {
			return err
		}
		ids = ids[n:]
		for _, f := range folders {
			rel := byID[f.DrivewsID]
			for _, it := range f.Items {
				if it.IsDir() {
					continue
				}
				name := sanitizeName(it.FullName())
				childRel := name
				if rel != "" {
					childRel = rel + "/" + name
				}
				prev, ok := e.st.Drive()[childRel]
				if !ok {
					continue
				}
				prev.Etag = it.Etag
				prev.DrivewsID = it.DrivewsID
				prev.DocwsID = it.DocwsID
				prev.Zone = it.Zone
				prev.RemoteMTime = it.ModTime()
				e.st.PutDrive(childRel, prev)
			}
		}
	}
	return e.st.Save()
}

// conflictPath builds "name.conflict-<stamp>.ext" next to the original.
func conflictPath(p, stamp string) string {
	dir := filepath.Dir(p)
	base := filepath.Base(p)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return filepath.Join(dir, fmt.Sprintf("%s.conflict-%s%s", stem, stamp, ext))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
