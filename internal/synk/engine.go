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

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

// folderBatch is how many folders are requested per API call while walking.
const folderBatch = 30

// Engine performs synchronisation passes.
type driveClient interface {
	ListFolders(ctx context.Context, drivewsIDs []string) ([]icloud.Folder, error)
	Download(ctx context.Context, item icloud.Item, dst io.Writer) (int64, error)
	Upload(ctx context.Context, zone, parentDocwsID, name string, size int64, modTime time.Time, src io.Reader) error
	CreateFolder(ctx context.Context, parentDrivewsID, name string) (icloud.Item, error)
	MoveToTrash(ctx context.Context, items []icloud.Item) error
	ListPhotos(ctx context.Context, pageSize int, fn func(icloud.Photo) error) error
	DownloadPhoto(ctx context.Context, p icloud.Photo, w io.Writer) (int64, error)
}

// renameFile is replaceable in tests to simulate filesystem errors.
var renameFile = os.Rename

type Engine struct {
	cfg *config.Config
	cl  driveClient
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

	// replacementSkipLogged prevents repeated log messages for retained
	// internal replacement files during the lifetime of this engine.
	replacementSkipLogged sync.Map
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

	local, skipped, oversized, err := e.scanner.Scan()
	if err != nil {
		return stats, fmt.Errorf("scanning %s: %w", e.cfg.DriveDir(), err)
	}
	for _, s := range skipped {
		e.log.Debug("skipping local entry", "path", s)
	}

	protectedPaths := make(map[string]bool, len(oversized))
	for _, p := range oversized {
		protectedPaths[p] = true
		e.log.Warn("protecting oversized local file from reconciliation actions", "path", p)
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
		DeleteLocal:      e.cfg.DeleteLocal,
		DeleteRemote:     e.cfg.DeleteRemote,
		ProtectDeletions: e.cfg.ProtectDeletions,
		ProtectedPaths:   protectedPaths,
		Conflict:         e.cfg.Conflict,
	})

	e.log.Debug("drive pass planned", "operations", len(ops))

	if e.dryRun {
		for _, op := range ops {
			e.log.Info("would "+string(op.Kind),
				"path", op.Path,
				"note", op.Note)
			stats.Skipped++
		}
		return stats, nil
	}

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
	// When a subfolder is configured, resolve it first and treat it as the
	// local mirror root; never walk or reconcile its siblings.
	rootID := icloud.RootFolderID
	if e.cfg.DriveSubfolder != "" {
		root, err := e.resolveRemoteSubfolder(ctx, e.cfg.DriveSubfolder)
		if err != nil {
			return nil, nil, err
		}
		rootID = root.DrivewsID
		dirs[""] = root
	}
	pending := map[string]string{rootID: ""}

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
		// Every requested folder must be returned. Otherwise the remote
		// inventory is incomplete, so abort instead of syncing against it.
		for _, id := range ids {
			if _, ok := pending[id]; ok {
				return nil, nil, fmt.Errorf(
					"iCloud returned no result for requested folder %q; refusing to sync with incomplete inventory",
					id,
				)
			}
		}

		for id, rel := range next {
			pending[id] = rel
		}
	}
	return files, dirs, nil
}

// resolveRemoteSubfolder finds a configured path beneath the iCloud Drive root.
// It deliberately fails if any component is missing or is not a folder; uploads
// must never silently create the configured mirror root at the wrong location.
func (e *Engine) resolveRemoteSubfolder(ctx context.Context, subfolder string) (icloud.Item, error) {
	currentID := icloud.RootFolderID
	var current icloud.Item
	for _, component := range strings.Split(subfolder, "/") {
		folders, err := e.cl.ListFolders(ctx, []string{currentID})
		if err != nil {
			return icloud.Item{}, fmt.Errorf("locating iCloud Drive folder %q: %w", subfolder, err)
		}
		found := false
		for _, folder := range folders {
			if folder.DrivewsID != currentID {
				continue
			}
			for _, item := range folder.Items {
				if sanitizeName(item.FullName()) == component && item.IsDir() {
					current = item
					currentID = item.DrivewsID
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return icloud.Item{}, fmt.Errorf("configured iCloud Drive folder %q was not found (missing folder %q); refusing to sync", subfolder, component)
		}
	}
	return current, nil
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
func (e *Engine) logSkip(op Op) {
	if op.Note == "internal replacement file retained; automatic reconciliation skipped" {
		if _, alreadyLogged := e.replacementSkipLogged.LoadOrStore(op.Path, struct{}{}); alreadyLogged {
			return
		}
		e.log.Info("skipping", "path", op.Path, "reason", op.Note)
		return
	}

	e.log.Warn("skipping", "path", op.Path, "reason", op.Note)
}

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
			if e.dryRun {
				e.log.Info("would update sync state", "path", op.Path)
				stats.Skipped++
				continue
			}
			e.recordSynced(op)

		case OpTrashLocal:
			trashLocal = append(trashLocal, op)

		case OpTrashRemote:
			trashRemote = append(trashRemote, op)

		case OpSkip:
			e.logSkip(op)
			stats.Skipped++

		case OpForget:
			if e.dryRun {
				e.log.Info("would forget sync state", "path", op.Path)
				stats.Skipped++
				continue
			}
			if e.cfg.ProtectDeletions {
				e.log.Warn("preserving sync state because deletion protection is enabled",
					"path", op.Path)
				stats.Skipped++
				continue
			}
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
		e.log.Info("download started", "path", op.Path)
		var n int64
		n, err = e.download(ctx, op, passStamp)
		if err == nil {
			stats.Downloaded++
			stats.Bytes += n
			e.log.Info("downloaded", "path", op.Path, "bytes", n)
		}
	case OpUpload:
		e.log.Info("upload started", "path", op.Path)
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
	if op.Remote.Size > 0 && n != op.Remote.Size {
		return 0, fmt.Errorf(
			"incomplete download for %s: received %d bytes, expected %d",
			op.Path, n, op.Remote.Size,
		)
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}

	remoteHash := hex.EncodeToString(h.Sum(nil))

	// If the downloaded content matches the existing local file, preserve
	// the local file and its timestamp. Only refresh the sync metadata.
	if !op.Conflict {
		if prev, ok := e.st.Drive()[op.Path]; ok &&
			prev.SHA256 != "" &&
			prev.SHA256 == remoteHash {
			info, statErr := os.Stat(dst)
			if statErr == nil && info.Mode().IsRegular() {
				localHash, hashErr := fileSHA256(dst)
				if hashErr == nil && localHash == remoteHash {
					e.st.PutDrive(op.Path, Entry{
						Size:        info.Size(),
						LocalMTime:  info.ModTime(),
						SHA256:      remoteHash,
						Etag:        op.Remote.Etag,
						DrivewsID:   op.Remote.DrivewsID,
						DocwsID:     op.Remote.DocwsID,
						Zone:        op.Remote.Zone,
						RemoteMTime: op.Remote.ModTime(),
					})
					return 0, nil
				}
			}
		}
	}

	// Preserve the local version only after confirming the remote content
	// differs and the download has completed successfully.
	// Prepare the downloaded file before touching the existing local file.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return 0, err
	}

	// For conflicts, preserve the original version and restore it if the
	// final replacement fails.
	var backupPath string
	if op.Conflict && op.Winner == WinnerRemote {
		switch e.cfg.Conflict {
		case "both":
			var err error
			backupPath, err = createConflictCopy(dst, conflictPath(dst, passStamp))
			if err != nil && !os.IsNotExist(err) {
				return 0, fmt.Errorf("preserving local conflict version: %w", err)
			}
		case "remote":
			backupPath = filepath.Join(
				filepath.Dir(dst),
				"."+filepath.Base(dst)+".icloud-synk-backup-"+passStamp,
			)
		}

		if backupPath != "" && e.cfg.Conflict != "both" {
			if err := renameFile(dst, backupPath); err != nil && !os.IsNotExist(err) {
				return 0, fmt.Errorf("preserving local conflict version: %w", err)
			}
		}
	}

	if err := renameFile(tmpName, dst); err != nil {
		// If replacement failed, restore the original file when possible.
		if backupPath != "" {
			if e.cfg.Conflict == "both" {
				// The original remains at dst; remove only the extra hard link.
				if removeErr := os.Remove(backupPath); removeErr != nil && !os.IsNotExist(removeErr) {
					return 0, fmt.Errorf(
						"replacing downloaded file failed: %v; removing temporary conflict copy failed: %w",
						err, removeErr,
					)
				}
			} else if restoreErr := renameFile(backupPath, dst); restoreErr != nil {
				return 0, fmt.Errorf(
					"replacing downloaded file failed: %v; restoring original file failed: %w",
					err, restoreErr,
				)
			}
		}
		return 0, err
	}

	if backupPath != "" {
		if e.cfg.Conflict == "both" {
			e.log.Warn(
				"kept local version alongside",
				"path", op.Path,
				"as", filepath.Base(backupPath),
			)
		} else if e.cfg.Conflict == "remote" {
			if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
				e.log.Warn(
					"could not remove temporary conflict backup",
					"path", backupPath,
					"err", err,
				)
			}
		}
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
		SHA256:      remoteHash,
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

	// The remote version loses a conflict: preserve it before uploading.
	if op.Conflict && op.Winner == WinnerLocal && e.cfg.Conflict == "both" && op.Remote.DocwsID != "" {
		cp := conflictPath(src, passStamp)
		if err := e.downloadTo(ctx, op.Remote, cp); err != nil {
			return 0, fmt.Errorf("preserving iCloud conflict version for %q: %w", op.Path, err)
		}
		e.log.Warn("kept iCloud version alongside", "path", op.Path, "as", filepath.Base(cp))
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
	prev := e.st.Drive()[op.Path]

	// Only enter replacement mode when reconciliation identified an existing
	// remote file at this exact path. Refresh its metadata before renaming it.
	var existing *icloud.Item
	if op.Remote.DrivewsID != "" && op.Remote.FullName() == name {
		items, listErr := e.remoteChildren(ctx, parent)
		if listErr != nil {
			return 0, fmt.Errorf("listing remote parent before replacement: %w", listErr)
		}
		if item := findRemoteItem(items, name); item != nil {
			copy := *item
			existing = &copy
		} else {
			return 0, fmt.Errorf(
				"cannot safely replace %q: expected existing remote file %q was not found; refusing normal upload",
				op.Path, name,
			)
		}
	}

	uploadName := name
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	if existing != nil {
		if _, ok := e.cl.(interface {
			Rename(context.Context, icloud.Item, string) error
		}); !ok {
			return 0, fmt.Errorf("cannot safely replace %q: iCloud client does not support rename", op.Path)
		}
		uploadName = replacementSiblingName(name, "new", stamp)
	}

	var uploadErr error
	delays := []time.Duration{0, time.Second, 3 * time.Second}

	for attempt, delay := range delays {
		if delay > 0 {
			time.Sleep(delay)
		}

		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		h.Reset()

		uploadErr = e.cl.Upload(
			ctx,
			parent.Zone,
			parent.DocwsID,
			uploadName,
			info.Size(),
			info.ModTime(),
			io.TeeReader(f, h),
		)

		if uploadErr == nil {
			break
		}

		if !strings.Contains(uploadErr.Error(), "ZONE_BUSY") {
			break
		}

		e.log.Warn("iCloud zone busy, retrying upload",
			"path", op.Path,
			"attempt", attempt+1,
		)
	}

	if uploadErr != nil {
		return 0, uploadErr
	}

	var remoteAfter *icloud.Item
	if existing != nil {
		expectedHash := hex.EncodeToString(h.Sum(nil))

		// Confirm the temporary upload and its content before touching the old
		// file. On every failure, leave all remote versions in place.
		tempItem, err := e.findRemoteItemRetry(ctx, parent, uploadName)
		if err != nil {
			return 0, fmt.Errorf("temporary upload for %q was not confirmed; existing file retained: %w", op.Path, err)
		}
		if err := e.verifyRemoteHash(ctx, *tempItem, expectedHash); err != nil {
			return 0, fmt.Errorf("temporary upload for %q failed content verification; existing file retained: %w", op.Path, err)
		}

		backupName := replacementSiblingName(name, "backup", stamp)
		backupItem, err := e.renameRemoteAndConfirm(ctx, parent, *existing, backupName)
		if err != nil {
			return 0, fmt.Errorf("could not confirm backup of %q; remote versions were retained where possible: %w", op.Path, err)
		}

		target, err := e.renameRemoteAndConfirm(ctx, parent, *tempItem, name)
		if err != nil {
			// Restore the old name only if the target name is currently absent.
			// Never overwrite an uncertain remote state.
			items, listErr := e.remoteChildren(ctx, parent)
			if listErr == nil && findRemoteItem(items, name) == nil {
				if currentBackup := findRemoteItem(items, backupName); currentBackup != nil {
					if _, rollbackErr := e.renameRemoteAndConfirm(ctx, parent, *currentBackup, name); rollbackErr != nil {
						e.log.Error("could not restore remote file after replacement failure",
							"path", op.Path, "backup", backupName, "error", rollbackErr)
					}
				}
			}
			return 0, fmt.Errorf("new version could not be confirmed at original name %q; backup %q retained: %w", op.Path, backupName, err)
		}

		if verifyErr := e.verifyRemoteHash(ctx, target, expectedHash); verifyErr != nil {
			// Preserve the unverified new version under a separate name before
			// restoring the old version to the original path.
			failedName := replacementSiblingName(name, "failed", stamp)
			failedItem, moveErr := e.renameRemoteAndConfirm(ctx, parent, target, failedName)
			if moveErr != nil {
				e.log.Error("could not move unverified replacement aside",
					"path", op.Path, "backup", backupName, "error", moveErr)
				return 0, fmt.Errorf("replacement content verification failed for %q; backup %q retained, but unverified version could not be moved aside: %w",
					op.Path, backupName, verifyErr)
			}

			items, listErr := e.remoteChildren(ctx, parent)
			if listErr != nil {
				e.log.Error("could not list remote folder before restoring backup",
					"path", op.Path, "backup", backupName, "failed_version", failedItem.FullName(), "error", listErr)
				return 0, fmt.Errorf("replacement content verification failed for %q; old backup %q and unverified version %q retained: %w",
					op.Path, backupName, failedItem.FullName(), verifyErr)
			}

			if findRemoteItem(items, name) != nil {
				return 0, fmt.Errorf("replacement content verification failed for %q; original name is occupied, old backup %q and unverified version %q retained: %w",
					op.Path, backupName, failedItem.FullName(), verifyErr)
			}

			currentBackup := findRemoteItem(items, backupName)
			if currentBackup == nil {
				return 0, fmt.Errorf("replacement content verification failed for %q; backup %q was not found, unverified version %q retained: %w",
					op.Path, backupName, failedItem.FullName(), verifyErr)
			}

			if _, restoreErr := e.renameRemoteAndConfirm(ctx, parent, *currentBackup, name); restoreErr != nil {
				e.log.Error("could not restore old remote version after verification failure",
					"path", op.Path, "backup", backupName, "failed_version", failedItem.FullName(), "error", restoreErr)
				return 0, fmt.Errorf("replacement content verification failed for %q; backup %q and unverified version %q retained, restoration failed: %w",
					op.Path, backupName, failedItem.FullName(), verifyErr)
			}

			return 0, fmt.Errorf("replacement content verification failed for %q; old version restored, unverified version retained as %q: %w",
				op.Path, failedItem.FullName(), verifyErr)
		}

		remoteAfter = &target
		e.log.Info("remote file replaced and verified; previous version retained as backup",
			"path", op.Path, "backup", backupItem.FullName())
	}

	// The new etag is unknown until the folder is re-listed; refreshEtags does
	// that once per pass. Keep the previous ids so the entry stays usable.
	drivewsID, docwsID := prev.DrivewsID, prev.DocwsID
	if remoteAfter != nil {
		drivewsID = remoteAfter.DrivewsID
		docwsID = remoteAfter.DocwsID
	}
	e.st.PutDrive(op.Path, Entry{
		Size:        info.Size(),
		LocalMTime:  info.ModTime(),
		SHA256:      hex.EncodeToString(h.Sum(nil)),
		Etag:        "", // refreshed below
		DrivewsID:   drivewsID,
		DocwsID:     docwsID,
		Zone:        parent.Zone,
		RemoteMTime: info.ModTime(),
	})
	e.markStale(parentRel)
	return info.Size(), nil
}

// remoteChildren returns the direct children of a remote folder.
func (e *Engine) remoteChildren(ctx context.Context, parent icloud.Item) ([]icloud.Item, error) {
	if parent.DrivewsID == "" {
		return nil, fmt.Errorf("remote parent has no drivewsid")
	}
	folders, err := e.cl.ListFolders(ctx, []string{parent.DrivewsID})
	if err != nil {
		return nil, err
	}
	for _, folder := range folders {
		if folder.DrivewsID == parent.DrivewsID {
			return folder.Items, nil
		}
	}
	return nil, fmt.Errorf("remote parent %q was not returned by iCloud", parent.DrivewsID)
}

func findRemoteItem(items []icloud.Item, name string) *icloud.Item {
	for i := range items {
		if items[i].FullName() == name {
			return &items[i]
		}
	}
	return nil
}

func replacementSiblingName(name, kind, stamp string) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s.icloud-synk-%s-%s%s", stem, kind, stamp, ext)
}

// findRemoteItemRetry allows for short delays in iCloud's listing visibility.
func (e *Engine) findRemoteItemRetry(ctx context.Context, parent icloud.Item, name string) (*icloud.Item, error) {
	delays := []time.Duration{0, 300 * time.Millisecond, 700 * time.Millisecond}
	var lastListErr error
	listSucceeded := false

	for _, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		items, err := e.remoteChildren(ctx, parent)
		if err != nil {
			lastListErr = err
			continue
		}
		listSucceeded = true
		if item := findRemoteItem(items, name); item != nil {
			copy := *item
			return &copy, nil
		}
	}

	if !listSucceeded && lastListErr != nil {
		return nil, fmt.Errorf("could not confirm exact remote name %q after retries; last listing error: %w", name, lastListErr)
	}
	if lastListErr != nil {
		return nil, fmt.Errorf("exact remote name %q was not found after retries; at least one listing attempt failed: %w", name, lastListErr)
	}
	return nil, fmt.Errorf("exact remote name %q was not found after retries", name)
}

func (e *Engine) verifyRemoteHash(ctx context.Context, item icloud.Item, expectedHash string) error {
	h := sha256.New()
	if _, err := e.cl.Download(ctx, item, h); err != nil {
		return err
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expectedHash {
		return fmt.Errorf("SHA-256 mismatch: expected %s, received %s", expectedHash, actual)
	}
	return nil
}

// renameRemoteAndConfirm retries iCloud listing because a server-side rename
// may succeed even when the API response or an immediate listing fails.
func (e *Engine) renameRemoteAndConfirm(ctx context.Context, parent icloud.Item, item icloud.Item, newName string) (icloud.Item, error) {
	renamer, ok := e.cl.(interface {
		Rename(context.Context, icloud.Item, string) error
	})
	if !ok {
		return icloud.Item{}, fmt.Errorf("iCloud client does not support rename")
	}

	renameErr := renamer.Rename(ctx, item, newName)
	delays := []time.Duration{0, 300 * time.Millisecond, 700 * time.Millisecond}
	var lastListErr error
	var listingSucceeded bool

	for attempt, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return icloud.Item{}, fmt.Errorf("confirmation of rename to %q interrupted: %w", newName, ctx.Err())
			case <-time.After(delay):
			}
		}

		items, listErr := e.remoteChildren(ctx, parent)
		if listErr != nil {
			lastListErr = listErr
			continue
		}
		listingSucceeded = true

		if found := findRemoteItem(items, newName); found != nil {
			if item.DrivewsID == "" || found.DrivewsID != item.DrivewsID {
				return icloud.Item{}, fmt.Errorf(
					"rename to %q found an unexpected item ID: expected %q, got %q",
					newName, item.DrivewsID, found.DrivewsID,
				)
			}
			return *found, nil
		}

		if attempt < len(delays)-1 {
			e.log.Debug("remote rename not visible yet; retrying confirmation",
				"name", newName, "attempt", attempt+1)
		}
	}

	if !listingSucceeded {
		if renameErr != nil {
			return icloud.Item{}, fmt.Errorf(
				"rename returned %v and all confirmation listings failed: %w",
				renameErr, lastListErr,
			)
		}
		return icloud.Item{}, fmt.Errorf(
			"rename was reported successful but all confirmation listings failed: %w",
			lastListErr,
		)
	}

	if renameErr != nil {
		return icloud.Item{}, fmt.Errorf(
			"rename to %q returned an error and the target was not found after retries: %w",
			newName, renameErr,
		)
	}
	return icloud.Item{}, fmt.Errorf(
		"rename to %q was reported successful but the name was not found after retries",
		newName,
	)
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

	// Create the destination without overwriting an existing file.
	// The temporary file is in the same directory, so a hard link is atomic.
	for i := 0; ; i++ {
		candidate := dst
		if i > 0 {
			base := filepath.Base(dst)
			ext := filepath.Ext(base)
			stem := strings.TrimSuffix(base, ext)
			candidate = filepath.Join(filepath.Dir(dst), fmt.Sprintf("%s (%d)%s", stem, i, ext))
		}

		if err := os.Link(tmpName, candidate); err != nil {
			if os.IsExist(err) {
				continue
			}
			return fmt.Errorf("creating conflict copy without overwriting %q: %w", candidate, err)
		}
		return nil
	}
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

// createConflictCopy creates a hard link to src at a unique conflict filename.
// It never replaces an existing destination and leaves src untouched.
func createConflictCopy(src, preferred string) (string, error) {
	if _, err := os.Stat(src); err != nil {
		return "", err
	}

	for i := 0; ; i++ {
		candidate := preferred
		if i > 0 {
			base := filepath.Base(preferred)
			ext := filepath.Ext(base)
			stem := strings.TrimSuffix(base, ext)
			candidate = filepath.Join(filepath.Dir(preferred), fmt.Sprintf("%s (%d)%s", stem, i, ext))
		}

		if err := os.Link(src, candidate); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		return candidate, nil
	}
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
