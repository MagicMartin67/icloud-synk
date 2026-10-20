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
	folders       map[string]icloud.Folder
	data          map[string][]byte
	failList      bool
	afterList     func()
	afterDownload func()
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
	if f.afterList != nil {
		f.afterList()
	}
	return out, nil
}

func (f *verifyFakeClient) Download(_ context.Context, item icloud.Item, w io.Writer) (int64, error) {
	data, ok := f.data[item.DocwsID]
	if !ok {
		return 0, errors.New("download missing")
	}
	n, err := io.Copy(w, bytes.NewReader(data))
	if f.afterDownload != nil {
		f.afterDownload()
	}
	return n, err
}

func TestVerifyDriveReturnsCanceledContext(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Drive"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cfg := &config.Config{TargetDir: root}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{},
		data:    map[string][]byte{},
	}

	_, err := VerifyDrive(ctx, cfg, client)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestVerifyDriveStopsWhenContextCanceledDuringListing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Drive"), 0o755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := &config.Config{TargetDir: root}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{
			icloud.RootFolderID: {
				Item: icloud.Item{DrivewsID: icloud.RootFolderID},
			},
		},
		data:      map[string][]byte{},
		afterList: cancel,
	}

	_, err := VerifyDrive(ctx, cfg, client)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled after listing, got %v", err)
	}
}

func TestVerifyDriveReportsLocalFileChangedDuringVerification(t *testing.T) {
	root := t.TempDir()
	drive := filepath.Join(root, "Drive")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}

	localPath := filepath.Join(drive, "hello.txt")
	if err := os.WriteFile(localPath, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{TargetDir: root}
	item := icloud.Item{
		DrivewsID: "file-1",
		DocwsID:   "doc-1",
		Name:      "hello",
		Extension: "txt",
		Type:      icloud.TypeFile,
		Size:      5,
	}

	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{
			icloud.RootFolderID: {
				Item:  icloud.Item{DrivewsID: icloud.RootFolderID},
				Items: []icloud.Item{item},
			},
		},
		data: map[string][]byte{"doc-1": []byte("hello")},
		afterDownload: func() {
			if err := os.WriteFile(localPath, []byte("changed"), 0o600); err != nil {
				t.Errorf("could not change local file during verification: %v", err)
			}
		},
	}

	report, err := VerifyDrive(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unverifiable) != 1 ||
		!strings.Contains(report.Unverifiable[0], "änderte sich während der Prüfung") {
		t.Fatalf("expected changed local file to be unverifiable, got %+v", report)
	}
	if report.Complete() {
		t.Fatal("verification must not be complete when the local file changes")
	}
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

func TestVerifyDriveAbortsOnSanitizedPathCollision(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Drive"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{TargetDir: root}
	first := icloud.Item{
		DrivewsID: "file-1", DocwsID: "doc-1",
		Name: " hello", Extension: "txt", Type: icloud.TypeFile, Size: 5,
	}
	second := icloud.Item{
		DrivewsID: "file-2", DocwsID: "doc-2",
		Name: "hello", Extension: "txt", Type: icloud.TypeFile, Size: 5,
	}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{
			icloud.RootFolderID: {
				Item:  icloud.Item{DrivewsID: icloud.RootFolderID},
				Items: []icloud.Item{first, second},
			},
		},
		data: map[string][]byte{
			"doc-1": []byte("hello"),
			"doc-2": []byte("world"),
		},
	}

	report, err := VerifyDrive(context.Background(), cfg, client)
	if err == nil || !strings.Contains(err.Error(), "iCloud path collision") {
		t.Fatalf("expected path collision error, got report=%+v err=%v", report, err)
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
	if err == nil || !strings.Contains(err.Error(), "inventory incomplete") {
		t.Fatalf("expected incomplete inventory error, got %v", err)
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

func TestVerifyDriveAbortsOnAmbiguousConfiguredSubfolder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Drive"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		TargetDir:      root,
		DriveSubfolder: "Tappe/Dokumente sync NAS",
	}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{
			icloud.RootFolderID: {
				Item: icloud.Item{DrivewsID: icloud.RootFolderID},
				Items: []icloud.Item{
					{DrivewsID: "folder-tappe-1", Name: "Tappe", Type: icloud.TypeFolder},
					{DrivewsID: "folder-tappe-2", Name: "Tappe", Type: icloud.TypeFolder},
				},
			},
			"folder-tappe-1": {
				Item: icloud.Item{DrivewsID: "folder-tappe-1"},
			},
			"folder-tappe-2": {
				Item: icloud.Item{DrivewsID: "folder-tappe-2"},
			},
		},
		data: map[string][]byte{},
	}

	_, err := VerifyDrive(context.Background(), cfg, client)
	if err == nil || !strings.Contains(err.Error(), "is ambiguous") {
		t.Fatalf("expected ambiguous configured subfolder error, got %v", err)
	}
}

func TestVerifyDriveDetectsLocalFileMissingInRemote(t *testing.T) {
	root := t.TempDir()
	drive := filepath.Join(root, "Drive")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drive, "local-only.txt"), []byte("local"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{TargetDir: root}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{
			icloud.RootFolderID: {
				Item: icloud.Item{DrivewsID: icloud.RootFolderID},
			},
		},
		data: map[string][]byte{},
	}

	report, err := VerifyDrive(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete() {
		t.Fatal("local-only file must not produce a complete verification")
	}
	if len(report.Differences) != 1 || !strings.Contains(report.Differences[0], "local-only.txt") {
		t.Fatalf("expected local-only file difference, got %+v", report)
	}
}

func TestVerifyDriveDetectsRemoteFileMissingLocally(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Drive"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{TargetDir: root}
	item := icloud.Item{
		DrivewsID: "file-remote-only",
		DocwsID:   "doc-remote-only",
		Name:      "remote-only",
		Extension: "txt",
		Type:      icloud.TypeFile,
		Size:      6,
	}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{
			icloud.RootFolderID: {
				Item:  icloud.Item{DrivewsID: icloud.RootFolderID},
				Items: []icloud.Item{item},
			},
		},
		data: map[string][]byte{"doc-remote-only": []byte("remote")},
	}

	report, err := VerifyDrive(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete() {
		t.Fatal("remote-only file must not produce a complete verification")
	}
	if len(report.Differences) != 1 || !strings.Contains(report.Differences[0], "remote-only.txt") {
		t.Fatalf("expected remote-only file difference, got %+v", report)
	}
}

func TestVerifyDriveAbortsOnCyclicFolderReference(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Drive"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{TargetDir: root}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{
			icloud.RootFolderID: {
				Item: icloud.Item{DrivewsID: icloud.RootFolderID},
				Items: []icloud.Item{{
					DrivewsID: "folder-a",
					Name:      "folder-a",
					Type:      icloud.TypeFolder,
				}},
			},
			"folder-a": {
				Item: icloud.Item{DrivewsID: "folder-a"},
				Items: []icloud.Item{{
					DrivewsID: "folder-a",
					Name:      "self-reference",
					Type:      icloud.TypeFolder,
				}},
			},
		},
		data: map[string][]byte{},
	}

	_, err := VerifyDrive(context.Background(), cfg, client)
	if err == nil || !strings.Contains(err.Error(), "referenced at multiple paths") {
		t.Fatalf("expected cyclic folder reference error, got %v", err)
	}
}

func TestVerifyDriveDetectsFolderDifferences(t *testing.T) {
	t.Run("local folder missing remotely", func(t *testing.T) {
		root := t.TempDir()
		drive := filepath.Join(root, "Drive")
		if err := os.MkdirAll(filepath.Join(drive, "local-folder"), 0o755); err != nil {
			t.Fatal(err)
		}

		cfg := &config.Config{TargetDir: root}
		client := &verifyFakeClient{
			folders: map[string]icloud.Folder{
				icloud.RootFolderID: {
					Item: icloud.Item{DrivewsID: icloud.RootFolderID},
				},
			},
			data: map[string][]byte{},
		}

		report, err := VerifyDrive(context.Background(), cfg, client)
		if err != nil {
			t.Fatal(err)
		}
		if report.Complete() {
			t.Fatal("local-only folder must not produce a complete verification")
		}
		if len(report.Differences) != 1 || !strings.Contains(report.Differences[0], "local-folder") {
			t.Fatalf("expected local-only folder difference, got %+v", report)
		}
	})

	t.Run("remote folder missing locally", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "Drive"), 0o755); err != nil {
			t.Fatal(err)
		}

		cfg := &config.Config{TargetDir: root}
		client := &verifyFakeClient{
			folders: map[string]icloud.Folder{
				icloud.RootFolderID: {
					Item: icloud.Item{DrivewsID: icloud.RootFolderID},
					Items: []icloud.Item{{
						DrivewsID: "folder-remote-only",
						Name:      "remote-folder",
						Type:      icloud.TypeFolder,
					}},
				},
				"folder-remote-only": {
					Item: icloud.Item{DrivewsID: "folder-remote-only"},
				},
			},
			data: map[string][]byte{},
		}

		report, err := VerifyDrive(context.Background(), cfg, client)
		if err != nil {
			t.Fatal(err)
		}
		if report.Complete() {
			t.Fatal("remote-only folder must not produce a complete verification")
		}
		if len(report.Differences) != 1 || !strings.Contains(report.Differences[0], "remote-folder") {
			t.Fatalf("expected remote-only folder difference, got %+v", report)
		}
	})
}

func TestVerifyDriveReportsUnexpectedRemoteDownloadSize(t *testing.T) {
	root := t.TempDir()
	drive := filepath.Join(root, "Drive")
	if err := os.MkdirAll(drive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drive, "size.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{TargetDir: root}
	item := icloud.Item{
		DrivewsID: "file-size",
		DocwsID:   "doc-size",
		Name:      "size",
		Extension: "txt",
		Type:      icloud.TypeFile,
		Size:      99,
	}
	client := &verifyFakeClient{
		folders: map[string]icloud.Folder{
			icloud.RootFolderID: {
				Item:  icloud.Item{DrivewsID: icloud.RootFolderID},
				Items: []icloud.Item{item},
			},
		},
		data: map[string][]byte{"doc-size": []byte("hello")},
	}

	report, err := VerifyDrive(context.Background(), cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete() {
		t.Fatal("unexpected remote download size must not produce a complete verification")
	}
	if len(report.Unverifiable) != 1 || !strings.Contains(report.Unverifiable[0], "size.txt") {
		t.Fatalf("expected remote size verification error, got %+v", report)
	}
}
