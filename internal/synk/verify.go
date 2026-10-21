package synk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

// driveVerifyClient exposes only read-only operations required by verification.
type driveVerifyClient interface {
	ListFolders(context.Context, []string) ([]icloud.Folder, error)
	Download(context.Context, icloud.Item, io.Writer) (int64, error)
}

// VerifyReport contains the outcome of a read-only local/remote comparison.
type VerifyReport struct {
	LocalFiles   int
	RemoteFiles  int
	Identical    int
	Differences  []string
	Unverifiable []string
	SkippedLocal []string
}

// Complete reports whether every relevant file could be compared and matched.
func (r VerifyReport) Complete() bool {
	return len(r.Differences) == 0 && len(r.Unverifiable) == 0 &&
		len(r.SkippedLocal) == 0 && r.Identical == r.LocalFiles &&
		r.LocalFiles == r.RemoteFiles
}

// VerifyDrive inventories the configured iCloud folder and compares file hashes.
// It does not create directories, invoke reconciliation, or read/write sync state.
func VerifyDrive(ctx context.Context, cfg *config.Config, cl driveVerifyClient) (VerifyReport, error) {
	var report VerifyReport
	if err := ctx.Err(); err != nil {
		return report, err
	}
	scanner := &Scanner{Root: cfg.DriveDir(), Exclude: cfg.Exclude}
	local, skipped, _, err := scanner.Scan()
	if err != nil {
		return report, fmt.Errorf("local inventory incomplete: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	report.SkippedLocal = append(report.SkippedLocal, skipped...)

	localFiles := make(map[string]LocalFile)
	localDirs := make(map[string]bool)
	for p, entry := range local {
		if entry.IsDir {
			localDirs[p] = true
		} else {
			localFiles[p] = entry
		}
	}
	report.LocalFiles = len(localFiles)

	rootID := icloud.RootFolderID
	if cfg.DriveSubfolder != "" {
		parts := strings.Split(cfg.DriveSubfolder, "/")
		for _, part := range parts {
			folders, err := cl.ListFolders(ctx, []string{rootID})
			if ctxErr := ctx.Err(); ctxErr != nil {
				return report, ctxErr
			}
			if err != nil {
				return report, fmt.Errorf("resolving iCloud folder %q: %w", part, err)
			}
			var matchedID string
			matches := 0
			for _, folder := range folders {
				if folder.DrivewsID != rootID {
					continue
				}
				for _, item := range folder.Items {
					if item.IsDir() && sanitizeName(item.FullName()) == part {
						matchedID = item.DrivewsID
						matches++
					}
				}
			}
			if matches == 0 {
				return report, fmt.Errorf("configured iCloud folder %q not found; verification aborted", cfg.DriveSubfolder)
			}
			if matches > 1 {
				return report, fmt.Errorf(
					"configured iCloud folder component %q is ambiguous (%d matches); verification aborted",
					part, matches,
				)
			}
			rootID = matchedID
		}
	}

	remoteFiles := make(map[string]icloud.Item)
	remoteDirs := make(map[string]bool)
	remotePaths := make(map[string]string)
	pending := map[string]string{rootID: ""}
	visited := map[string]string{rootID: ""}
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		ids := make([]string, 0, len(pending))
		for id := range pending {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) > folderBatch {
			ids = ids[:folderBatch]
		}
		folders, err := cl.ListFolders(ctx, ids)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return report, ctxErr
		}
		if err != nil {
			return report, fmt.Errorf("listing iCloud Drive: %w", err)
		}
		next := make(map[string]string)
		seen := make(map[string]bool)
		for _, folder := range folders {
			parentRel, ok := pending[folder.DrivewsID]
			if !ok {
				continue
			}
			seen[folder.DrivewsID] = true
			for _, item := range folder.Items {
				name := sanitizeName(item.FullName())
				if name == "" {
					continue
				}
				rel := name
				if parentRel != "" {
					rel = parentRel + "/" + name
				}
				if scanner.IsExcluded(rel) {
					continue
				}
				if previousID, exists := remotePaths[rel]; exists && previousID != item.DrivewsID {
					return report, fmt.Errorf(
						"iCloud path collision after sanitizing %q (items %q and %q); verification aborted",
						rel, previousID, item.DrivewsID,
					)
				}
				remotePaths[rel] = item.DrivewsID
				if item.IsDir() {
					remoteDirs[rel] = true
					if previousPath, exists := visited[item.DrivewsID]; exists {
						if previousPath != rel {
							return report, fmt.Errorf(
								"iCloud folder %q referenced at multiple paths %q and %q; verification aborted",
								item.DrivewsID, previousPath, rel,
							)
						}
					} else {
						visited[item.DrivewsID] = rel
						next[item.DrivewsID] = rel
					}
				} else {
					remoteFiles[rel] = item
				}
			}
		}
		for _, id := range ids {
			if !seen[id] {
				return report, fmt.Errorf("iCloud returned no result for folder %q; inventory incomplete", id)
			}
			delete(pending, id)
		}
		for id, rel := range next {
			pending[id] = rel
		}
	}
	report.RemoteFiles = len(remoteFiles)

	for p := range localDirs {
		if !remoteDirs[p] {
			report.Differences = append(report.Differences, "Ordner fehlt in iCloud: "+p)
		}
	}
	for p := range remoteDirs {
		if !localDirs[p] {
			report.Differences = append(report.Differences, "Ordner fehlt lokal: "+p)
		}
	}
	for p := range localFiles {
		if _, ok := remoteFiles[p]; !ok {
			report.Differences = append(report.Differences, "Datei fehlt in iCloud: "+p)
		}
	}
	for p := range remoteFiles {
		if _, ok := localFiles[p]; !ok {
			report.Differences = append(report.Differences, "Datei fehlt lokal: "+p)
		}
	}

	paths := make([]string, 0, len(localFiles))
	for p := range localFiles {
		if _, ok := remoteFiles[p]; ok {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		item := remoteFiles[p]
		if item.Type == icloud.TypePackage || item.ItemCount > 0 {
			report.Unverifiable = append(report.Unverifiable, "Paketinhalt nicht direkt vergleichbar: "+p)
			continue
		}
		localPath := filepath.Join(cfg.DriveDir(), filepath.FromSlash(p))
		before, err := os.Stat(localPath)
		if err != nil {
			report.Unverifiable = append(report.Unverifiable, fmt.Sprintf("Lokal nicht statbar (%s): %v", p, err))
			continue
		}
		localHash, err := hashLocalFile(ctx, localPath)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return report, ctxErr
			}
			report.Unverifiable = append(report.Unverifiable, fmt.Sprintf("Lokal nicht lesbar (%s): %v", p, err))
			continue
		}
		remoteHash := sha256.New()
		n, err := cl.Download(ctx, item, remoteHash)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return report, ctxErr
		}
		if err != nil {
			report.Unverifiable = append(report.Unverifiable, fmt.Sprintf("iCloud-Abruf fehlgeschlagen (%s): %v", p, err))
			continue
		}
		if item.Size > 0 && n != item.Size {
			report.Unverifiable = append(report.Unverifiable, fmt.Sprintf("iCloud-Größe unerwartet (%s): %d statt %d Bytes", p, n, item.Size))
			continue
		}
		after, err := os.Stat(localPath)
		if err != nil {
			report.Unverifiable = append(report.Unverifiable, fmt.Sprintf("Lokale Datei nach dem Lesen nicht statbar (%s): %v", p, err))
			continue
		}
		if !os.SameFile(before, after) || before.Size() != after.Size() ||
			!before.ModTime().Equal(after.ModTime()) {
			report.Unverifiable = append(report.Unverifiable, "Lokale Datei änderte sich während der Prüfung: "+p)
			continue
		}
		if after.Size() != n || localHash != hex.EncodeToString(remoteHash.Sum(nil)) {
			report.Differences = append(report.Differences, "Inhalt oder Größe weicht ab: "+p)
			continue
		}
		report.Identical++
	}
	sort.Strings(report.Differences)
	sort.Strings(report.Unverifiable)
	sort.Strings(report.SkippedLocal)
	return report, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func hashLocalFile(ctx context.Context, p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, contextReader{ctx: ctx, reader: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
