package synk

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

func TestApplyOpForgetPreservesStateWhenDeletionProtectionEnabled(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")

	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}

	st.PutDrive("old.txt", Entry{
		Size:      12,
		Etag:      "test-etag",
		DrivewsID: "test-drive-id",
	})

	cfg := &config.Config{
		TargetDir:        t.TempDir(),
		ProtectDeletions: true,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, nil, st, logger, false)

	stats := engine.apply(context.Background(), []Op{
		{Kind: OpForget, Path: "old.txt"},
	})

	if got, ok := st.Drive()["old.txt"]; !ok {
		t.Fatal("sync state was removed despite deletion protection")
	} else if got.DrivewsID != "test-drive-id" || got.Etag != "test-etag" {
		t.Fatalf("sync state changed unexpectedly: %+v", got)
	}

	if stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", stats.Skipped)
	}
}

type oversizedFileClient struct {
	incompleteFolderClient
}

func (oversizedFileClient) ListFolders(
	ctx context.Context,
	drivewsIDs []string,
) ([]icloud.Folder, error) {
	now := time.Now().Truncate(time.Second)
	folders := make([]icloud.Folder, 0, len(drivewsIDs))

	for _, id := range drivewsIDs {
		folders = append(folders, icloud.Folder{
			Item: icloud.Item{
				DrivewsID: id,
				Type:      icloud.TypeFolder,
			},
			Items: []icloud.Item{
				remoteFile("large.bin", "e1", 20, now),
			},
		})
	}

	return folders, nil
}

func TestSyncDriveProtectsOversizedLocalFile(t *testing.T) {
	target := t.TempDir()
	cfg := &config.Config{
		TargetDir:    target,
		MaxFileSize:  10,
		DeleteLocal:  true,
		DeleteRemote: true,
		Conflict:     "both",
	}

	if err := os.MkdirAll(cfg.DriveDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.MetaDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	localPath := filepath.Join(cfg.DriveDir(), "large.bin")
	if err := os.WriteFile(localPath, []byte("this file is larger than ten bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := LoadState(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	st.PutDrive("large.bin", Entry{
		Size: 20, LocalMTime: now, Etag: "e1", DrivewsID: "remote-id",
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, nil, st, logger, false)
	engine.cl = oversizedFileClient{}

	stats, err := engine.SyncDrive(context.Background())
	if err != nil {
		t.Fatalf("SyncDrive returned an error: %v", err)
	}

	if stats.TrashedLoc != 0 || stats.TrashedRem != 0 ||
		stats.Downloaded != 0 || stats.Uploaded != 0 {
		t.Fatalf("oversized file caused a transfer or deletion: %+v", stats)
	}
	if stats.Skipped == 0 {
		t.Fatal("expected the oversized file to be skipped")
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("oversized local file was changed or removed: %v", err)
	}
	if _, ok := st.Drive()["large.bin"]; !ok {
		t.Fatal("sync state for oversized file was removed")
	}
}

type dryRunClient struct {
	incompleteFolderClient
	downloadCalls int
	uploadCalls   int
	trashCalls    int
}

func (c *dryRunClient) ListFolders(
	ctx context.Context,
	drivewsIDs []string,
) ([]icloud.Folder, error) {
	now := time.Now().Truncate(time.Second)
	folders := make([]icloud.Folder, 0, len(drivewsIDs))

	for _, id := range drivewsIDs {
		folder := icloud.Folder{
			Item: icloud.Item{
				DrivewsID: id,
				Type:      icloud.TypeFolder,
			},
		}

		if len(folders) == 0 {
			folder.Items = []icloud.Item{
				remoteFile("dry-run-test.txt", "etag-new", 12, now),
			}
		}

		folders = append(folders, folder)
	}

	return folders, nil
}

func (c *dryRunClient) Download(
	ctx context.Context,
	item icloud.Item,
	w io.Writer,
) (int64, error) {
	c.downloadCalls++
	return 0, nil
}

func (c *dryRunClient) Upload(
	ctx context.Context,
	zone, folderDocwsID, name string,
	size int64,
	mtime time.Time,
	r io.Reader,
) error {
	c.uploadCalls++
	return nil
}

func (c *dryRunClient) MoveToTrash(
	ctx context.Context,
	items []icloud.Item,
) error {
	c.trashCalls++
	return nil
}

func TestSyncDriveDryRunDoesNotModifyStateOrTransferFiles(t *testing.T) {
	target := t.TempDir()
	cfg := &config.Config{
		TargetDir: target,
		Conflict:  "both",
	}

	if err := os.MkdirAll(cfg.DriveDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.MetaDir(), 0o755); err != nil {
		t.Fatal(err)
	}

	statePath := cfg.StatePath()
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}

	st.PutDrive("existing.txt", Entry{
		Size:      10,
		Etag:      "etag-existing",
		DrivewsID: "existing-id",
	})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	stateBefore, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	client := &dryRunClient{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, nil, st, logger, true)
	engine.cl = client

	_, err = engine.SyncDrive(context.Background())
	if err != nil {
		t.Fatalf("SyncDrive dry-run returned an error: %v", err)
	}

	stateAfter, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if string(stateBefore) != string(stateAfter) {
		t.Fatal("state file changed during dry-run")
	}

	if client.downloadCalls != 0 ||
		client.uploadCalls != 0 ||
		client.trashCalls != 0 {
		t.Fatalf(
			"dry-run executed file operations: downloads=%d uploads=%d trash=%d",
			client.downloadCalls,
			client.uploadCalls,
			client.trashCalls,
		)
	}
}
