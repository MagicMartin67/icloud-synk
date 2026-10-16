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
	content                 string
	err                     error
	calls                   int
	uploadCalls             int
	remoteItems             []icloud.Item
	remoteContent           map[string]string
	downloadOverrideName    string
	downloadOverrideContent string
	listErrAfterUpload      error
}

func (c *downloadTestClient) ListFolders(
	_ context.Context, ids []string,
) ([]icloud.Folder, error) {
	if c.uploadCalls > 0 && c.listErrAfterUpload != nil {
		return nil, c.listErrAfterUpload
	}
	if c.remoteItems == nil {
		return nil, nil
	}
	var result []icloud.Folder
	for _, id := range ids {
		result = append(result, icloud.Folder{
			Item:  icloud.Item{DrivewsID: id, Type: icloud.TypeFolder},
			Items: append([]icloud.Item(nil), c.remoteItems...),
		})
	}
	return result, nil
}

func (c *downloadTestClient) Download(
	_ context.Context, item icloud.Item, dst io.Writer,
) (int64, error) {
	c.calls++
	if c.err != nil {
		return 0, c.err
	}
	content := c.content
	if value, ok := c.remoteContent[item.DrivewsID]; ok {
		content = value
	}
	if c.downloadOverrideName != "" &&
		strings.HasPrefix(item.DrivewsID, "uploaded-") &&
		item.FullName() == c.downloadOverrideName {
		content = c.downloadOverrideContent
	}
	n, err := io.WriteString(dst, content)
	return int64(n), err
}

func (c *downloadTestClient) Upload(
	_ context.Context, zone, parentDocwsID, name string,
	size int64, mtime time.Time, src io.Reader,
) error {
	c.uploadCalls++
	if c.remoteItems == nil {
		return nil
	}
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	ext := filepath.Ext(name)
	item := icloud.Item{
		DrivewsID:    "uploaded-" + name,
		DocwsID:      "uploaded-doc-" + name,
		Zone:         zone,
		Name:         strings.TrimSuffix(name, ext),
		Extension:    strings.TrimPrefix(ext, "."),
		Etag:         "uploaded-etag-" + name,
		Type:         icloud.TypeFile,
		Size:         size,
		DateModified: mtime,
	}
	c.remoteItems = append(c.remoteItems, item)
	if c.remoteContent == nil {
		c.remoteContent = make(map[string]string)
	}
	c.remoteContent[item.DrivewsID] = string(data)
	return nil
}

func (c *downloadTestClient) Rename(
	_ context.Context, item icloud.Item, newName string,
) error {
	ext := filepath.Ext(newName)
	for i := range c.remoteItems {
		if c.remoteItems[i].DrivewsID == item.DrivewsID {
			c.remoteItems[i].Name = strings.TrimSuffix(newName, ext)
			c.remoteItems[i].Extension = strings.TrimPrefix(ext, ".")
			c.remoteItems[i].Etag = "renamed-" + newName
			return nil
		}
	}
	return os.ErrNotExist
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

func TestUploadAbortsWhenTemporaryUploadCannotBeConfirmed(t *testing.T) {
	client := &downloadTestClient{
		content:            "bisherige iCloud-Version",
		listErrAfterUpload: os.ErrPermission,
	}
	engine, _, _ := newDownloadTestEngine(t, client, "neue lokale Version")

	engine.putDir("", icloud.Item{
		Name:      "root",
		Type:      icloud.TypeFolder,
		DrivewsID: "root-id",
		DocwsID:   "root-doc-id",
		Zone:      icloud.DefaultZone,
	})

	remote := icloud.Item{
		Name:      "test.txt",
		Type:      icloud.TypeFile,
		Etag:      "etag-alt",
		DrivewsID: "remote-id",
		DocwsID:   "doc-id",
		Zone:      icloud.DefaultZone,
	}
	client.remoteItems = []icloud.Item{remote}
	client.remoteContent = map[string]string{
		remote.DrivewsID: client.content,
	}

	op := Op{
		Kind:     OpUpload,
		Path:     "test.txt",
		Conflict: true,
		Winner:   WinnerLocal,
		Remote:   remote,
	}

	_, err := engine.upload(context.Background(), op, "test")
	if err == nil {
		t.Fatal("erwarteter Fehler bei fehlender Bestätigung des temporären Uploads")
	}
	if !strings.Contains(err.Error(), "temporary upload") {
		t.Fatalf("Fehler benennt den fehlenden Upload-Nachweis nicht: %v", err)
	}
	if client.uploadCalls != 1 {
		t.Fatalf("Upload-Aufrufe = %d, erwartet 1", client.uploadCalls)
	}

	var originalFound bool
	for _, item := range client.remoteItems {
		if item.DrivewsID == remote.DrivewsID && item.FullName() == "test.txt" {
			originalFound = true
		}
		if item.DrivewsID == remote.DrivewsID && item.FullName() != "test.txt" {
			t.Fatalf("alte Remote-Datei wurde unerwartet umbenannt: %q", item.FullName())
		}
	}
	if !originalFound {
		t.Fatal("alte Remote-Datei ist nicht mehr unter ihrem ursprünglichen Namen vorhanden")
	}

	content, ok := client.remoteContent[remote.DrivewsID]
	if !ok || content != "bisherige iCloud-Version" {
		t.Fatalf("Inhalt der alten Remote-Datei wurde verändert: %q", content)
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
	client.remoteItems = []icloud.Item{op.Remote}
	client.remoteContent = map[string]string{
		op.Remote.DrivewsID: client.content,
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

	if client.calls != 3 {
		t.Errorf("Download-Aufrufe = %d, erwartet 3", client.calls)
	}
	if client.uploadCalls != 1 {
		t.Errorf("Upload-Aufrufe = %d, erwartet 1", client.uploadCalls)
	}
}

func TestUploadConflictBothRestoresOldVersionWhenFinalHashFails(t *testing.T) {
	client := &downloadTestClient{
		content:                 "bisherige iCloud-Version",
		downloadOverrideName:    "test.txt",
		downloadOverrideContent: "fehlerhafter Inhalt",
	}
	engine, _, _ := newDownloadTestEngine(t, client, "neue lokale Version")
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
	client.remoteItems = []icloud.Item{op.Remote}
	client.remoteContent = map[string]string{
		op.Remote.DrivewsID: client.content,
	}

	_, err := engine.upload(context.Background(), op, "test")
	if err == nil {
		t.Fatal("erwarteter Fehler bei der abschließenden Inhaltsprüfung fehlt")
	}

	items, listErr := engine.remoteChildren(context.Background(), icloud.Item{
		DrivewsID: "root-id",
		Type:      icloud.TypeFolder,
	})
	if listErr != nil {
		t.Fatal(listErr)
	}

	original := findRemoteItem(items, "test.txt")
	if original == nil {
		t.Fatal("ursprünglicher Dateiname fehlt nach fehlgeschlagener Inhaltsprüfung")
	}
	if original.DrivewsID != "remote-id" {
		t.Fatalf("ursprünglicher Name zeigt auf %q statt auf die alte Datei", original.DrivewsID)
	}
	if got := client.remoteContent[original.DrivewsID]; got != "bisherige iCloud-Version" {
		t.Fatalf("alte Version wurde nicht wiederhergestellt: %q", got)
	}

	var failedVersionFound bool
	for _, item := range items {
		if !strings.Contains(item.FullName(), ".icloud-synk-failed-") {
			continue
		}
		failedVersionFound = true
		if got := client.remoteContent[item.DrivewsID]; got != "neue lokale Version" {
			t.Fatalf("gespeicherter Inhalt der fehlerhaften Version unerwartet: %q", got)
		}
	}
	if !failedVersionFound {
		t.Fatal("fehlerhafte neue Version wurde nach der Wiederherstellung nicht aufbewahrt")
	}
}

type renameFailureTestClient struct {
	*downloadTestClient
}

func (c *renameFailureTestClient) Rename(
	ctx context.Context, item icloud.Item, newName string,
) error {
	// Simuliert, dass weder die neue Datei noch die alte Sicherung
	// auf den ursprünglichen Namen umbenannt werden können.
	if newName == "test.txt" {
		return os.ErrPermission
	}
	return c.downloadTestClient.Rename(ctx, item, newName)
}

func TestUploadConflictBothRetainsBackupWhenRestoreFails(t *testing.T) {
	base := &downloadTestClient{
		content: "bisherige iCloud-Version",
	}
	engine, _, _ := newDownloadTestEngine(t, base, "neue lokale Version")

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
	base.remoteItems = []icloud.Item{op.Remote}
	base.remoteContent = map[string]string{
		op.Remote.DrivewsID: "bisherige iCloud-Version",
	}
	engine.cl = &renameFailureTestClient{downloadTestClient: base}

	_, err := engine.upload(context.Background(), op, "test")
	if err == nil {
		t.Fatal("erwarteter Fehler beim Umbenennen und Wiederherstellen fehlt")
	}

	items, listErr := engine.remoteChildren(context.Background(), icloud.Item{
		DrivewsID: "root-id",
		Type:      icloud.TypeFolder,
	})
	if listErr != nil {
		t.Fatal(listErr)
	}

	if original := findRemoteItem(items, "test.txt"); original != nil {
		t.Fatalf("unerwartete Datei unter ursprünglichem Namen: %q", original.DrivewsID)
	}

	var backup *icloud.Item
	for i := range items {
		if strings.Contains(items[i].FullName(), ".icloud-synk-backup-") {
			backup = &items[i]
			break
		}
	}
	if backup == nil {
		t.Fatal("Sicherungsdatei fehlt nach fehlgeschlagener Wiederherstellung")
	}
	if got := base.remoteContent[backup.DrivewsID]; got != "bisherige iCloud-Version" {
		t.Fatalf("alter Dateiinhalt wurde nicht erhalten: %q", got)
	}
}

type renameIdentityTestClient struct {
	*downloadTestClient
}

func (*renameIdentityTestClient) Rename(
	context.Context, icloud.Item, string,
) error {
	// Simuliert einen erfolgreichen Rename-Aufruf, während unter
	// dem Zielnamen tatsächlich eine andere Datei liegt.
	return nil
}

type renameAppliedButErrorTestClient struct {
	*downloadTestClient
}

func (c *renameAppliedButErrorTestClient) Rename(
	ctx context.Context, item icloud.Item, newName string,
) error {
	if err := c.downloadTestClient.Rename(ctx, item, newName); err != nil {
		return err
	}
	// Die Umbenennung wurde ausgeführt, aber die API meldet einen Fehler.
	return os.ErrPermission
}

func TestRenameRemoteAndConfirmAcceptsRenameDespiteAPIError(t *testing.T) {
	base := &downloadTestClient{
		remoteItems: []icloud.Item{
			{
				Name:      "quelle",
				Extension: "txt",
				Type:      icloud.TypeFile,
				DrivewsID: "datei-id",
			},
		},
	}
	client := &renameAppliedButErrorTestClient{downloadTestClient: base}
	engine, _, _ := newDownloadTestEngine(t, base, "lokaler Inhalt")
	engine.cl = client

	parent := icloud.Item{
		Name:      "root",
		Type:      icloud.TypeFolder,
		DrivewsID: "root-id",
	}
	original := icloud.Item{
		Name:      "quelle",
		Extension: "txt",
		Type:      icloud.TypeFile,
		DrivewsID: "datei-id",
	}

	got, err := engine.renameRemoteAndConfirm(
		context.Background(), parent, original, "ziel.txt",
	)
	if err != nil {
		t.Fatalf("tatsächlich ausgeführte Umbenennung wurde nicht erkannt: %v", err)
	}
	if got.FullName() != "ziel.txt" {
		t.Fatalf("Dateiname = %q, erwartet %q", got.FullName(), "ziel.txt")
	}
	if got.DrivewsID != original.DrivewsID {
		t.Fatalf("DrivewsID = %q, erwartet %q", got.DrivewsID, original.DrivewsID)
	}
}

type renameAppliedListFailureTestClient struct {
	*downloadTestClient
	listCalls       int
	failAfterRename bool
}

func (c *renameAppliedListFailureTestClient) Rename(
	ctx context.Context, item icloud.Item, newName string,
) error {
	if err := c.downloadTestClient.Rename(ctx, item, newName); err != nil {
		return err
	}
	c.failAfterRename = true
	return nil
}

func (c *renameAppliedListFailureTestClient) ListFolders(
	ctx context.Context, ids []string,
) ([]icloud.Folder, error) {
	if c.failAfterRename {
		c.listCalls++
		return nil, os.ErrPermission
	}
	return c.downloadTestClient.ListFolders(ctx, ids)
}

func TestUploadRetainsBackupAndTemporaryFileWhenBackupConfirmationFails(t *testing.T) {
	base := &downloadTestClient{
		content: "bisherige iCloud-Version",
		remoteItems: []icloud.Item{
			{
				Name:      "test",
				Extension: "txt",
				Type:      icloud.TypeFile,
				Etag:      "etag-alt",
				DrivewsID: "remote-id",
				DocwsID:   "doc-id",
				Zone:      icloud.DefaultZone,
			},
		},
		remoteContent: map[string]string{
			"remote-id": "bisherige iCloud-Version",
		},
	}
	client := &renameAppliedListFailureTestClient{downloadTestClient: base}
	engine, _, _ := newDownloadTestEngine(t, base, "neue lokale Version")
	engine.cl = client
	engine.putDir("", icloud.Item{
		Name:      "root",
		Type:      icloud.TypeFolder,
		DrivewsID: "root-id",
		DocwsID:   "root-doc-id",
		Zone:      icloud.DefaultZone,
	})

	remote := base.remoteItems[0]
	op := Op{
		Kind:     OpUpload,
		Path:     "test.txt",
		Conflict: true,
		Winner:   WinnerLocal,
		Remote:   remote,
	}

	_, err := engine.upload(context.Background(), op, "test")
	if err == nil {
		t.Fatal("erwarteter Fehler bei fehlgeschlagener Backup-Bestätigung blieb aus")
	}
	if !strings.Contains(err.Error(), "could not confirm backup") {
		t.Fatalf("Fehler benennt die fehlgeschlagene Backup-Bestätigung nicht: %v", err)
	}
	if client.listCalls != 3 {
		t.Fatalf("fehlgeschlagene Bestätigungsabfragen = %d, erwartet 3", client.listCalls)
	}
	if base.uploadCalls != 1 {
		t.Fatalf("Upload-Aufrufe = %d, erwartet 1", base.uploadCalls)
	}

	var oldFound, tempFound, originalNameFound bool
	for _, item := range base.remoteItems {
		switch {
		case item.DrivewsID == remote.DrivewsID:
			oldFound = true
			if item.FullName() == "test.txt" {
				originalNameFound = true
			}
			if got := base.remoteContent[item.DrivewsID]; got != "bisherige iCloud-Version" {
				t.Fatalf("Inhalt der alten Datei verändert: %q", got)
			}
		case strings.HasPrefix(item.DrivewsID, "uploaded-"):
			tempFound = true
			if item.FullName() == "test.txt" {
				originalNameFound = true
			}
			if got := base.remoteContent[item.DrivewsID]; got != "neue lokale Version" {
				t.Fatalf("Inhalt der temporären Datei unerwartet: %q", got)
			}
		}
	}
	if !oldFound {
		t.Fatal("alte iCloud-Datei bzw. ihr Backup ist nicht mehr vorhanden")
	}
	if !tempFound {
		t.Fatal("temporär hochgeladene neue Version ist nicht mehr vorhanden")
	}
	if originalNameFound {
		t.Fatal("unerwartet trägt eine Datei den ursprünglichen Namen test.txt")
	}
}

func TestRenameRemoteAndConfirmRetriesTransientListingFailure(t *testing.T) {
	base := &downloadTestClient{
		remoteItems: []icloud.Item{
			{
				Name:      "quelle",
				Extension: "txt",
				Type:      icloud.TypeFile,
				DrivewsID: "datei-id",
			},
		},
	}
	client := &renameAppliedTransientListFailureTestClient{
		downloadTestClient:    base,
		listFailuresRemaining: 1,
	}
	engine, _, _ := newDownloadTestEngine(t, base, "lokaler Inhalt")
	engine.cl = client

	parent := icloud.Item{
		Name:      "root",
		Type:      icloud.TypeFolder,
		DrivewsID: "root-id",
	}
	original := base.remoteItems[0]

	got, err := engine.renameRemoteAndConfirm(
		context.Background(), parent, original, "ziel.txt",
	)
	if err != nil {
		t.Fatalf("Umbenennung wurde nach vorübergehendem Listenfehler nicht bestätigt: %v", err)
	}
	if got.FullName() != "ziel.txt" {
		t.Fatalf("Dateiname = %q, erwartet %q", got.FullName(), "ziel.txt")
	}
	if got.DrivewsID != original.DrivewsID {
		t.Fatalf("DrivewsID = %q, erwartet %q", got.DrivewsID, original.DrivewsID)
	}
	if client.listCalls != 2 {
		t.Fatalf("Listenabrufe = %d, erwartet 2", client.listCalls)
	}
}

type renameAppliedTransientListFailureTestClient struct {
	*downloadTestClient
	listCalls             int
	listFailuresRemaining int
	failAfterRename       bool
}

func (c *renameAppliedTransientListFailureTestClient) Rename(
	ctx context.Context, item icloud.Item, newName string,
) error {
	if err := c.downloadTestClient.Rename(ctx, item, newName); err != nil {
		return err
	}
	c.failAfterRename = true
	return nil
}

func (c *renameAppliedTransientListFailureTestClient) ListFolders(
	ctx context.Context, ids []string,
) ([]icloud.Folder, error) {
	if c.failAfterRename {
		c.listCalls++
		if c.listFailuresRemaining > 0 {
			c.listFailuresRemaining--
			return nil, os.ErrPermission
		}
	}
	return c.downloadTestClient.ListFolders(ctx, ids)
}

func TestRenameRemoteAndConfirmReportsListingFailureAfterRename(t *testing.T) {
	base := &downloadTestClient{
		remoteItems: []icloud.Item{
			{
				Name:      "quelle",
				Extension: "txt",
				Type:      icloud.TypeFile,
				DrivewsID: "datei-id",
			},
		},
	}
	client := &renameAppliedListFailureTestClient{downloadTestClient: base}
	engine, _, _ := newDownloadTestEngine(t, base, "lokaler Inhalt")
	engine.cl = client

	parent := icloud.Item{
		Name:      "root",
		Type:      icloud.TypeFolder,
		DrivewsID: "root-id",
	}
	original := icloud.Item{
		Name:      "quelle",
		Extension: "txt",
		Type:      icloud.TypeFile,
		DrivewsID: "datei-id",
	}

	_, err := engine.renameRemoteAndConfirm(
		context.Background(), parent, original, "ziel.txt",
	)
	if err == nil {
		t.Fatal("erwarteter Fehler bei fehlgeschlagener Bestätigung blieb aus")
	}
	if !strings.Contains(err.Error(), "all confirmation listings failed") {
		t.Fatalf("Fehler kennzeichnet die fehlgeschlagene Bestätigung nicht: %v", err)
	}
	if client.listCalls != 3 {
		t.Fatalf("Anzahl fehlgeschlagener Listenabrufe = %d, erwartet 3", client.listCalls)
	}
	if got := base.remoteItems[0].FullName(); got != "ziel.txt" {
		t.Fatalf("Testvoraussetzung nicht erfüllt: Datei heißt %q statt ziel.txt", got)
	}
}

func TestRenameRemoteAndConfirmRejectsUnexpectedDrivewsID(t *testing.T) {
	base := &downloadTestClient{
		remoteItems: []icloud.Item{
			{
				Name:      "ziel",
				Extension: "txt",
				Type:      icloud.TypeFile,
				DrivewsID: "fremde-datei-id",
			},
		},
	}
	client := &renameIdentityTestClient{downloadTestClient: base}
	engine, _, _ := newDownloadTestEngine(t, base, "lokaler Inhalt")
	engine.cl = client

	parent := icloud.Item{
		Name:      "root",
		Type:      icloud.TypeFolder,
		DrivewsID: "root-id",
	}
	original := icloud.Item{
		Name:      "quelle",
		Extension: "txt",
		Type:      icloud.TypeFile,
		DrivewsID: "urspruengliche-datei-id",
	}

	_, err := engine.renameRemoteAndConfirm(
		context.Background(), parent, original, "ziel.txt",
	)
	if err == nil {
		t.Fatal("fremde Datei-ID wurde fälschlich als erfolgreiche Umbenennung akzeptiert")
	}
	if !strings.Contains(err.Error(), "unexpected item ID") {
		t.Fatalf("unerwartete Fehlermeldung: %v", err)
	}
}

type mixedListingFailureTestClient struct {
	*downloadTestClient
	calls int
}

func (c *mixedListingFailureTestClient) ListFolders(
	ctx context.Context, ids []string,
) ([]icloud.Folder, error) {
	c.calls++
	if c.calls == 1 {
		return nil, os.ErrPermission
	}
	return c.downloadTestClient.ListFolders(ctx, ids)
}

func TestFindRemoteItemRetryRetainsTransientListingError(t *testing.T) {
	base := &downloadTestClient{
		remoteItems: []icloud.Item{},
	}
	client := &mixedListingFailureTestClient{downloadTestClient: base}
	engine, _, _ := newDownloadTestEngine(t, base, "lokaler Inhalt")
	engine.cl = client

	parent := icloud.Item{
		Name:      "root",
		Type:      icloud.TypeFolder,
		DrivewsID: "root-id",
	}

	_, err := engine.findRemoteItemRetry(
		context.Background(), parent, "nicht-vorhanden.txt",
	)
	if err == nil {
		t.Fatal("erwarteter Fehler für nicht gefundene Datei fehlt")
	}
	if !strings.Contains(err.Error(), "nicht-vorhanden.txt") {
		t.Fatalf("Fehler nennt den gesuchten Dateinamen nicht: %v", err)
	}
	if !strings.Contains(err.Error(), os.ErrPermission.Error()) {
		t.Fatalf("vorheriger Listenfehler fehlt in der Diagnose: %v", err)
	}
	if client.calls != 3 {
		t.Fatalf("Listenabrufe = %d, erwartet 3", client.calls)
	}
}

func TestUploadAbortsWhenExpectedRemoteFileIsMissing(t *testing.T) {
	client := &downloadTestClient{
		content:     "bisherige iCloud-Version",
		remoteItems: []icloud.Item{},
	}
	engine, _, _ := newDownloadTestEngine(t, client, "neue lokale Version")
	engine.putDir("", icloud.Item{
		Name:      "root",
		Type:      icloud.TypeFolder,
		DrivewsID: "root-id",
		DocwsID:   "root-doc-id",
		Zone:      icloud.DefaultZone,
	})

	op := Op{
		Kind: OpUpload,
		Path: "test.txt",
		Remote: icloud.Item{
			Name:      "test.txt",
			Type:      icloud.TypeFile,
			DrivewsID: "remote-id",
			DocwsID:   "remote-doc-id",
			Zone:      icloud.DefaultZone,
		},
	}

	_, err := engine.upload(context.Background(), op, "test")
	if err == nil {
		t.Fatal("erwarteter Abbruch bei fehlender vorhandener iCloud-Datei blieb aus")
	}
	if !strings.Contains(err.Error(), "refusing normal upload") {
		t.Fatalf("unerwartete Fehlermeldung: %v", err)
	}
	if client.uploadCalls != 0 {
		t.Fatalf("Upload wurde trotz fehlender vorhandener Datei %d-mal aufgerufen", client.uploadCalls)
	}
	if len(client.remoteItems) != 0 {
		t.Fatalf("unerwartet wurden iCloud-Dateien angelegt: %d", len(client.remoteItems))
	}
}
