package synk

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

type incompleteFolderClient struct{}

func (incompleteFolderClient) ListFolders(
	ctx context.Context,
	drivewsIDs []string,
) ([]icloud.Folder, error) {
	// Simulate a successful API response that omits a requested folder.
	return []icloud.Folder{}, nil
}

func (incompleteFolderClient) Download(
	ctx context.Context,
	item icloud.Item,
	w io.Writer,
) (int64, error) {
	return 0, nil
}

func (incompleteFolderClient) Upload(
	ctx context.Context,
	zone, folderDocwsID, name string,
	size int64,
	mtime time.Time,
	r io.Reader,
) error {
	return nil
}

func (incompleteFolderClient) CreateFolder(
	ctx context.Context,
	parentDrivewsID, name string,
) (icloud.Item, error) {
	return icloud.Item{}, nil
}

func (incompleteFolderClient) MoveToTrash(
	ctx context.Context,
	items []icloud.Item,
) error {
	return nil
}

func (incompleteFolderClient) ListPhotos(
	ctx context.Context,
	pageSize int,
	fn func(icloud.Photo) error,
) error {
	return nil
}

func (incompleteFolderClient) DownloadPhoto(
	ctx context.Context,
	p icloud.Photo,
	w io.Writer,
) (int64, error) {
	return 0, nil
}

func TestFetchRemoteRejectsMissingFolderResponse(t *testing.T) {
	cfg := &config.Config{
		TargetDir: t.TempDir(),
	}
	st, err := LoadState(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, nil, st, logger, true)
	engine.cl = incompleteFolderClient{}

	_, _, err = engine.fetchRemote(context.Background())
	if err == nil {
		t.Fatal("expected an error when iCloud omits a requested folder")
	}
	if !strings.Contains(err.Error(), "incomplete inventory") {
		t.Fatalf("unexpected error: %v", err)
	}
}
