package synk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

type verifyFakeClient struct {
	folders  map[string]icloud.Folder
	data     map[string][]byte
	failList bool
}

func (f *verifyFakeClient) ListFolders(_ context.Context, ids []string) ([]icloud.Folder, error) {
	if f.failList {
		return nil, errors.New("listing failed")
	}
	var out []icloud.Folder
	for _, id := range ids {
		if folder, ok := f.folders[id]; ok {
			out = append(out, folder)
		}
	}
	return out, nil
}

func (f *verifyFakeClient) Download(_ context.Context, item icloud.Item, w io.Writer) (int64, error) {
	data, ok := f.data[item.DocwsID]
	if !ok {
		return 0, errors.New("download missing")
	}
	n, err := io.Copy(w, bytes.NewReader(data))
	return n, err
}

func TestVerifyDriveIdenticalFiles(t *testing.T) {
	root := t.TempDir()
	drive := filepath.Join(root, "Drive")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drive, "hello.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{TargetDir: root, Exclude: []string{"*.part"}}
	item := icloud.Item{DrivewsID: "file-1", DocwsID: "doc-1", Name: "hello", Extension: "txt", Type: icloud.TypeFile, Size: 5}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{
			icloud.RootFolderID: {Item: icloud.Item{DrivewsID: icloud.RootFolderID}, Items: []icloud.Item{item}},
		},
		data: map[string][]byte{"doc-1": []byte("hello")},
	}
	report, err := VerifyDrive(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete() {
		t.Fatalf("expected complete verification, got %+v", report)
	}
	if report.Identical != 1 {
		t.Fatalf("Identical=%d, want 1", report.Identical)
	}
	if _, err := os.Stat(cfg.StatePath()); !os.IsNotExist(err) {
		t.Fatalf("verification unexpectedly touched state path: %v", err)
	}
}

func TestVerifyDriveDetectsContentDifference(t *testing.T) {
	root := t.TempDir()
	drive := filepath.Join(root, "Drive")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drive, "hello.txt"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{TargetDir: root}
	item := icloud.Item{DrivewsID: "file-1", DocwsID: "doc-1", Name: "hello", Extension: "txt", Type: icloud.TypeFile, Size: 6}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{icloud.RootFolderID: {Item: icloud.Item{DrivewsID: icloud.RootFolderID}, Items: []icloud.Item{item}}},
		data:    map[string][]byte{"doc-1": []byte("remote")},
	}
	report, err := VerifyDrive(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Differences) != 1 || !strings.Contains(report.Differences[0], "hello.txt") {
		t.Fatalf("expected content difference, got %+v", report)
	}
	if report.Complete() {
		t.Fatal("mismatched files must not be complete")
	}
}

func TestVerifyDriveAbortsOnIncompleteInventory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Drive"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{TargetDir: root}
	client := &verifyFakeClient{folders: map[string]icloud.Folder{}, data: map[string][]byte{}}
	_, err := VerifyDrive(context.Background(), cfg, client)
	if err == nil {
		t.Fatal("expected incomplete inventory error")
	}
}

func TestVerifyDriveReportsUnreadableRemote(t *testing.T) {
	root := t.TempDir()
	drive := filepath.Join(root, "Drive")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drive, "hello.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{TargetDir: root}
	item := icloud.Item{DrivewsID: "file-1", DocwsID: "missing", Name: "hello", Extension: "txt", Type: icloud.TypeFile, Size: 5}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{icloud.RootFolderID: {Item: icloud.Item{DrivewsID: icloud.RootFolderID}, Items: []icloud.Item{item}}},
		data:    map[string][]byte{},
	}
	report, err := VerifyDrive(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unverifiable) != 1 {
		t.Fatalf("Unverifiable=%v, want one error", report.Unverifiable)
	}
	if report.Complete() {
		t.Fatal("download failure must not be complete")
	}
}

func TestVerifyDriveResolvesConfiguredSubfolder(t *testing.T) {
	root := t.TempDir()
	drive := filepath.Join(root, "Drive")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drive, "hello.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		TargetDir:      root,
		DriveSubfolder: "Tappe/Dokumente sync NAS",
	}
	file := icloud.Item{
		DrivewsID: "file-1", DocwsID: "doc-1",
		Name: "hello", Extension: "txt", Type: icloud.TypeFile, Size: 5,
	}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{
			icloud.RootFolderID: {
				Item: icloud.Item{DrivewsID: icloud.RootFolderID},
				Items: []icloud.Item{{
					DrivewsID: "folder-tappe", Name: "Tappe", Type: icloud.TypeFolder,
				}},
			},
			"folder-tappe": {
				Item: icloud.Item{DrivewsID: "folder-tappe"},
				Items: []icloud.Item{{
					DrivewsID: "folder-target", Name: "Dokumente sync NAS", Type: icloud.TypeFolder,
				}},
			},
			"folder-target": {
				Item:  icloud.Item{DrivewsID: "folder-target"},
				Items: []icloud.Item{file},
			},
		},
		data: map[string][]byte{"doc-1": []byte("hello")},
	}

	report, err := VerifyDrive(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete() {
		t.Fatalf("expected complete verification of configured subfolder, got %+v", report)
	}
}
