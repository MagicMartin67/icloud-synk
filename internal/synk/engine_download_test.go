package synk

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

type downloadTestClient struct {
	content     string
	err         error
	calls       int
	uploadCalls int
}

func (c *downloadTestClient) ListFolders(
	context.Context, []string,
) ([]icloud.Folder, error) {
	return nil, nil
}

func (c *downloadTestClient) Download(
	_ context.Context, _ icloud.Item, dst io.Writer,
) (int64, error) {
	c.calls++
	if c.err != nil {
		return 0, c.err
	}
	n, err := io.WriteString(dst, c.content)
	return int64(n), err
}

func (c *downloadTestClient) Upload(
	context.Context, string, string, string, int64, time.Time, io.Reader,
) error {
	c.uploadCalls++
	return nil
}

func (*downloadTestClient) CreateFolder(
	context.Context, string, string,
) (icloud.Item, error) {
	return icloud.Item{}, nil
}

func (*downloadTestClient) MoveToTrash(
	context.Context, []icloud.Item,
) error {
	return nil
}

func (*downloadTestClient) ListPhotos(
	context.Context, int, func(icloud.Photo) error,
) error {
	return nil
}

func (*downloadTestClient) DownloadPhoto(
	context.Context, icloud.Photo, io.Writer,
) (int64, error) {
	return 0, nil
}

func newDownloadTestEngine(
	t *testing.T,
	client *downloadTestClient,
	initialContent string,
) (*Engine, *State, string) {
	t.Helper()

	target := t.TempDir()
	cfg := &config.Config{
		TargetDir: target,
		Conflict:  "both",
	}
	state, err := LoadState(filepath.Join(target, ".icloud-synk", "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	engine := NewEngine(cfg, nil, state,
		slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	engine.cl = client

	dst := filepath.Join(cfg.DriveDir(), "test.txt")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte(initialContent), 0o644); err != nil {
		t.Fatal(err)
	}
	return engine, state, dst
}

func TestDownloadChangedETagSameContent(t *testing.T) {
	client := &downloadTestClient{content: "identischer Inhalt"}
	engine, state, dst := newDownloadTestEngine(t, client, "identischer Inhalt")

	oldMTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(dst, oldMTime, oldMTime); err != nil {
		t.Fatal(err)
	}
	oldHash, err := fileSHA256(dst)
	if err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	state.PutDrive("test.txt", Entry{
		Size:       int64(len("identischer Inhalt")),
		LocalMTime: oldMTime,
		SHA256:     oldHash,
		Etag:       "etag-alt",
		DrivewsID:  "remote-id",
	})

	// Der Remote-Zeitstempel unterscheidet sich bewusst vom lokalen.
	// Bei identischem Dateiinhalt darf die lokale Datei nicht ersetzt werden.
	remoteTime := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	op := Op{
		Kind: OpDownload,
		Path: "test.txt",
		Remote: icloud.Item{
			Name:         "test.txt",
			Type:         icloud.TypeFile,
			Size:         int64(len("identischer Inhalt")),
			Etag:         "etag-neu",
			DrivewsID:    "remote-id",
			DocwsID:      "doc-id",
			Zone:         icloud.DefaultZone,
			DateModified: remoteTime,
		},
	}

	_, err = engine.download(context.Background(), op, "test")
	if err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "identischer Inhalt" {
		t.Fatalf("Dateiinhalt verändert: %q", content)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(oldMTime) {
		t.Errorf("lokaler Zeitstempel verändert: %v", info.ModTime())
	}
	if !os.SameFile(oldInfo, info) {
		t.Error("lokale Datei wurde ersetzt, obwohl der Inhalt identisch ist")
	}
	if client.calls != 1 {
		t.Errorf("Download-Aufrufe = %d, erwartet 1", client.calls)
	}
	if got := state.Drive()["test.txt"].Etag; got != "etag-neu" {
		t.Errorf("gespeicherte ETag = %q, erwartet etag-neu", got)
	}
}

func TestDownloadChangedETagChangedContent(t *testing.T) {
	client := &downloadTestClient{content: "neuer Inhalt"}
	engine, _, dst := newDownloadTestEngine(t, client, "alter Inhalt")

	op := Op{
		Kind: OpDownload,
		Path: "test.txt",
		Remote: icloud.Item{
			Name:         "test.txt",
			Type:         icloud.TypeFile,
			Size:         int64(len("neuer Inhalt")),
			Etag:         "etag-neu",
			DrivewsID:    "remote-id",
			DocwsID:      "doc-id",
			Zone:         icloud.DefaultZone,
			DateModified: time.Now(),
		},
	}

	_, err := engine.download(context.Background(), op, "test")
	if err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "neuer Inhalt" {
		t.Fatalf("Dateiinhalt = %q, erwartet neuen Inhalt", content)
	}
}

func TestDownloadIncompleteContentPreservesLocalFile(t *testing.T) {
	client := &downloadTestClient{content: "kurzer"}
	engine, _, dst := newDownloadTestEngine(t, client, "wichtiger lokaler Inhalt")

	op := Op{
		Kind: OpDownload,
		Path: "test.txt",
		Remote: icloud.Item{
			Name:      "test.txt",
			Type:      icloud.TypeFile,
			Size:      int64(len("vollständiger erwarteter Inhalt")),
			Etag:      "etag-neu",
			DrivewsID: "remote-id",
		},
	}

	_, err := engine.download(context.Background(), op, "test")
	if err == nil {
		t.Fatal("erwarteter Fehler bei unvollständigem Download fehlt")
	}

	content, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("lokale Datei nicht mehr lesbar: %v", readErr)
	}
	if string(content) != "wichtiger lokaler Inhalt" {
		t.Fatalf("lokale Datei wurde verändert: %q", content)
	}
}

func TestDownloadEmptyFileSucceeds(t *testing.T) {
	client := &downloadTestClient{content: ""}
	engine, _, dst := newDownloadTestEngine(t, client, "alter Inhalt")

	op := Op{
		Kind: OpDownload,
		Path: "test.txt",
		Remote: icloud.Item{
			Name:      "test.txt",
			Type:      icloud.TypeFile,
			Size:      0,
			Etag:      "etag-empty",
			DrivewsID: "remote-id",
		},
	}

	_, err := engine.download(context.Background(), op, "test")
	if err != nil {
		t.Fatalf("Download einer leeren Datei fehlgeschlagen: %v", err)
	}

	content, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("leere Datei nicht lesbar: %v", readErr)
	}
	if len(content) != 0 {
		t.Fatalf("Dateiinhalt hat %d Bytes, erwartet 0", len(content))
	}
}

func TestDownloadErrorPreservesLocalFile(t *testing.T) {
	client := &downloadTestClient{err: os.ErrPermission}
	engine, _, dst := newDownloadTestEngine(t, client, "lokaler Inhalt")

	op := Op{
		Kind: OpDownload,
		Path: "test.txt",
		Remote: icloud.Item{
			Name:      "test.txt",
			Type:      icloud.TypeFile,
			Etag:      "etag-neu",
			DrivewsID: "remote-id",
		},
	}

	_, err := engine.download(context.Background(), op, "test")
	if err == nil {
		t.Fatal("erwarteter Downloadfehler fehlt")
	}

	content, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.EqualFold(string(content), "lokaler Inhalt") {
		t.Fatalf("lokale Datei wurde verändert: %q", content)
	}
}
func TestDownloadConflictErrorPreservesLocalFile(t *testing.T) {
	client := &downloadTestClient{err: os.ErrPermission}
	engine, _, dst := newDownloadTestEngine(t, client, "wichtige lokale Version")

	op := Op{
		Kind:     OpDownload,
		Path:     "test.txt",
		Conflict: true,
		Winner:   WinnerRemote,
		Remote: icloud.Item{
			Name:      "test.txt",
			Type:      icloud.TypeFile,
			Etag:      "etag-neu",
			DrivewsID: "remote-id",
			DocwsID:   "doc-id",
			Zone:      icloud.DefaultZone,
		},
	}

	_, err := engine.download(context.Background(), op, "test")
	if err == nil {
		t.Fatal("erwarteter Downloadfehler fehlt")
	}

	content, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("lokale Datei nicht mehr lesbar: %v", readErr)
	}
	if string(content) != "wichtige lokale Version" {
		t.Fatalf("lokale Version verloren oder verändert: %q", content)
	}

	entries, readErr := os.ReadDir(filepath.Dir(dst))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "test.conflict-") {
			t.Errorf("unerwartete Konfliktkopie trotz fehlgeschlagenem Download: %s", entry.Name())
		}
	}
}

func TestDownloadConflictPreservesExistingConflictCopy(t *testing.T) {
	client := &downloadTestClient{content: "neue iCloud-Version"}
	engine, _, dst := newDownloadTestEngine(t, client, "wichtige lokale Version")

	existing := conflictPath(dst, "test")
	const oldConflictContent = "bereits vorhandene Konfliktkopie"
	if err := os.WriteFile(existing, []byte(oldConflictContent), 0o644); err != nil {
		t.Fatal(err)
	}

	op := Op{
		Kind:     OpDownload,
		Path:     "test.txt",
		Conflict: true,
		Winner:   WinnerRemote,
		Remote: icloud.Item{
			Name:      "test.txt",
			Type:      icloud.TypeFile,
			Etag:      "etag-neu",
			DrivewsID: "remote-id",
			DocwsID:   "doc-id",
			Zone:      icloud.DefaultZone,
		},
	}

	if _, err := engine.download(context.Background(), op, "test"); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "neue iCloud-Version" {
		t.Fatalf("unerwarteter Inhalt der Zieldatei: %q", content)
	}

	content, err = os.ReadFile(existing)
	if err != nil {
		t.Fatalf("vorhandene Konfliktkopie wurde entfernt: %v", err)
	}
	if string(content) != oldConflictContent {
		t.Fatalf("vorhandene Konfliktkopie wurde verändert: %q", content)
	}
}

func TestDownloadConflictReplaceFailureRestoresOriginal(t *testing.T) {
	client := &downloadTestClient{content: "neue iCloud-Version"}
	engine, _, dst := newDownloadTestEngine(t, client, "wichtige lokale Version")

	originalRename := renameFile
	calls := 0
	renameFile = func(oldPath, newPath string) error {
		calls++
		// Der erste Aufruf simuliert einen Fehler beim Ersetzen.
		// Der zweite Aufruf stellt die lokale Version wieder her.
		if calls == 1 {
			return os.ErrPermission
		}
		return originalRename(oldPath, newPath)
	}
	t.Cleanup(func() {
		renameFile = originalRename
	})

	op := Op{
		Kind:     OpDownload,
		Path:     "test.txt",
		Conflict: true,
		Winner:   WinnerRemote,
		Remote: icloud.Item{
			Name:      "test.txt",
			Type:      icloud.TypeFile,
			Etag:      "etag-neu",
			DrivewsID: "remote-id",
			DocwsID:   "doc-id",
			Zone:      icloud.DefaultZone,
		},
	}

	_, err := engine.download(context.Background(), op, "test")
	if err == nil {
		t.Fatal("erwarteter Fehler beim Ersetzen fehlt")
	}

	content, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("Originaldatei wurde nicht wiederhergestellt: %v", readErr)
	}
	if string(content) != "wichtige lokale Version" {
		t.Fatalf("Originalinhalt verloren: %q", content)
	}

	entries, readDirErr := os.ReadDir(filepath.Dir(dst))
	if readDirErr != nil {
		t.Fatal(readDirErr)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".conflict-test") {
			t.Errorf("unnötige Konfliktkopie nach fehlgeschlagenem Ersetzen: %s", entry.Name())
		}
	}
}

func TestDownloadConflictRemoteRemovesBackupAfterSuccess(t *testing.T) {
	client := &downloadTestClient{content: "neue iCloud-Version"}
	engine, _, dst := newDownloadTestEngine(t, client, "wichtige lokale Version")
	engine.cfg.Conflict = "remote"

	op := Op{
		Kind:     OpDownload,
		Path:     "test.txt",
		Conflict: true,
		Winner:   WinnerRemote,
		Remote: icloud.Item{
			Name:      "test.txt",
			Type:      icloud.TypeFile,
			Etag:      "etag-neu",
			DrivewsID: "remote-id",
			DocwsID:   "doc-id",
			Zone:      icloud.DefaultZone,
		},
	}

	_, err := engine.download(context.Background(), op, "test")
	if err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "neue iCloud-Version" {
		t.Fatalf("unerwarteter Dateiinhalt: %q", content)
	}

	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".icloud-synk-backup-") {
			t.Errorf("unerwartete Sicherungsdatei nach erfolgreichem Austausch: %s", entry.Name())
		}
	}
}

func TestUploadConflictBothAbortsWhenRemoteBackupFails(t *testing.T) {
	client := &downloadTestClient{err: os.ErrPermission}
	engine, _, dst := newDownloadTestEngine(t, client, "wichtige lokale Version")

	engine.putDir("", icloud.Item{
		Name:      "root",
		Type:      icloud.TypeFolder,
		DrivewsID: "root-id",
		DocwsID:   "root-doc-id",
		Zone:      icloud.DefaultZone,
	})

	op := Op{
		Kind:     OpUpload,
		Path:     "test.txt",
		Conflict: true,
		Winner:   WinnerLocal,
		Remote: icloud.Item{
			Name:      "test.txt",
			Type:      icloud.TypeFile,
			Etag:      "etag-alt",
			DrivewsID: "remote-id",
			DocwsID:   "doc-id",
			Zone:      icloud.DefaultZone,
		},
	}

	_, err := engine.upload(context.Background(), op, "test")
	if err == nil {
		t.Fatal("erwarteter Fehler beim Sichern der iCloud-Konfliktkopie fehlt")
	}

	content, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("lokale Datei nicht mehr lesbar: %v", readErr)
	}
	if string(content) != "wichtige lokale Version" {
		t.Fatalf("lokaler Inhalt wurde verändert: %q", content)
	}
	if client.uploadCalls != 0 {
		t.Fatalf("Upload-Aufrufe = %d, erwartet 0", client.uploadCalls)
	}
}

func TestDownloadToPreservesExistingDestination(t *testing.T) {
	client := &downloadTestClient{content: "neue iCloud-Version"}
	engine, _, dst := newDownloadTestEngine(t, client, "lokale Version")

	existing := conflictPath(dst, "test")
	if err := os.WriteFile(existing, []byte("bereits vorhandene Konfliktkopie"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := engine.downloadTo(context.Background(), icloud.Item{
		Name: "test.txt",
		Type: icloud.TypeFile,
	}, existing)
	if err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "bereits vorhandene Konfliktkopie" {
		t.Fatalf("vorhandene Konfliktkopie wurde verändert: %q", content)
	}

	alternate := filepath.Join(filepath.Dir(existing),
		strings.TrimSuffix(filepath.Base(existing), filepath.Ext(existing))+" (1)"+filepath.Ext(existing))
	content, err = os.ReadFile(alternate)
	if err != nil {
		t.Fatalf("alternative Konfliktkopie fehlt: %v", err)
	}
	if string(content) != "neue iCloud-Version" {
		t.Fatalf("unerwarteter Inhalt der alternativen Konfliktkopie: %q", content)
	}
}

func TestUploadConflictBothPreservesRemoteAndUploadsLocal(t *testing.T) {
	client := &downloadTestClient{content: "bisherige iCloud-Version"}
	engine, _, dst := newDownloadTestEngine(t, client, "neue lokale Version")

	engine.putDir("", icloud.Item{
		Name:      "root",
		Type:      icloud.TypeFolder,
		DrivewsID: "root-id",
		DocwsID:   "root-doc-id",
		Zone:      icloud.DefaultZone,
	})

	op := Op{
		Kind:     OpUpload,
		Path:     "test.txt",
		Conflict: true,
		Winner:   WinnerLocal,
		Remote: icloud.Item{
			Name:      "test.txt",
			Type:      icloud.TypeFile,
			Etag:      "etag-alt",
			DrivewsID: "remote-id",
			DocwsID:   "doc-id",
			Zone:      icloud.DefaultZone,
		},
	}

	_, err := engine.upload(context.Background(), op, "test")
	if err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "neue lokale Version" {
		t.Fatalf("lokale Datei verändert: %q", content)
	}

	conflictPath := conflictPath(dst, "test")
	conflictContent, err := os.ReadFile(conflictPath)
	if err != nil {
		t.Fatalf("iCloud-Konfliktkopie fehlt: %v", err)
	}
	if string(conflictContent) != "bisherige iCloud-Version" {
		t.Fatalf("falscher Inhalt der Konfliktkopie: %q", conflictContent)
	}

	if client.calls != 1 {
		t.Errorf("Download-Aufrufe = %d, erwartet 1", client.calls)
	}
	if client.uploadCalls != 1 {
		t.Errorf("Upload-Aufrufe = %d, erwartet 1", client.uploadCalls)
	}
}
