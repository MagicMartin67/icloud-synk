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

type scopedFolderClient struct {
	incompleteFolderClient
}

func (scopedFolderClient) ListFolders(
	ctx context.Context,
	drivewsIDs []string,
) ([]icloud.Folder, error) {
	rootID := icloud.RootFolderID
	parentID := "FOLDER::com.apple.CloudDocs::parent"
	targetID := "FOLDER::com.apple.CloudDocs::target"
	siblingID := "FOLDER::com.apple.CloudDocs::sibling"

	var result []icloud.Folder
	for _, id := range drivewsIDs {
		switch id {
		case rootID:
			result = append(result, icloud.Folder{
				Item: icloud.Item{
					DrivewsID: rootID,
					Type:      icloud.TypeFolder,
				},
				Items: []icloud.Item{
					{
						DrivewsID: parentID,
						Name:      "Tappe",
						Type:      icloud.TypeFolder,
					},
					{
						DrivewsID: siblingID,
						Name:      "Nachbarordner",
						Type:      icloud.TypeFolder,
					},
				},
			})
		case parentID:
			result = append(result, icloud.Folder{
				Item: icloud.Item{
					DrivewsID: parentID,
					Type:      icloud.TypeFolder,
				},
				Items: []icloud.Item{
					{
						DrivewsID: targetID,
						Name:      "Dokumente sync NAS",
						Type:      icloud.TypeFolder,
					},
				},
			})
		case targetID:
			result = append(result, icloud.Folder{
				Item: icloud.Item{
					DrivewsID: targetID,
					Type:      icloud.TypeFolder,
				},
				Items: []icloud.Item{
					remoteFile("test 3.txt", "test-etag", 39, time.Now()),
				},
			})
		case siblingID:
			result = append(result, icloud.Folder{
				Item: icloud.Item{
					DrivewsID: siblingID,
					Type:      icloud.TypeFolder,
				},
				Items: []icloud.Item{
					remoteFile("nicht-uebernehmen.txt", "sibling-etag", 10, time.Now()),
				},
			})
		}
	}
	return result, nil
}

func TestFetchRemoteScopesConfiguredFolderAndPreservesSpaces(t *testing.T) {
	cfg := &config.Config{
		TargetDir:      t.TempDir(),
		DriveSubfolder: "Tappe/Dokumente sync NAS",
	}
	st, err := LoadState(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, nil, st, logger, true)
	engine.cl = scopedFolderClient{}

	files, _, err := engine.fetchRemote(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["test 3.txt"]; !ok {
		t.Fatalf("expected test 3.txt in remote inventory, got keys: %v", mapKeys(files))
	}
	if _, ok := files["nicht-uebernehmen.txt"]; ok {
		t.Fatal("file from sibling folder appeared in configured folder inventory")
	}
}

func mapKeys(items map[string]icloud.Item) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	return keys
}
