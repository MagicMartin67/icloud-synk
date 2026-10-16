package synk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// This test makes real changes in iCloud Drive. It is skipped unless explicitly
// enabled with ICLOUD_SYNK_LIVE_REPLACE=1. It never deletes remote files.
func TestLiveReplaceAgainstICloud(t *testing.T) {
	if os.Getenv("ICLOUD_SYNK_LIVE_REPLACE") != "1" {
		t.Skip("live iCloud test disabled; set ICLOUD_SYNK_LIVE_REPLACE=1 to enable")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	configDir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	cl, err := icloud.New(filepath.Join(configDir, "session.json"))
	if err != nil {
		t.Fatalf("loading saved iCloud session: %v", err)
	}
	if err := cl.Authenticate(ctx); err != nil {
		t.Fatalf("authenticating with saved iCloud session: %v", err)
	}

	cfg := config.Defaults()
	cfg.TargetDir = t.TempDir()
	cfg.DriveSubfolder = "Tappe/Dokumente sync NAS/Synk-test"
	cfg.SyncDrive = false
	cfg.SyncPhotos = false
	cfg.DeleteRemote = false
	cfg.DeleteLocal = false
	cfg.Conflict = "both"

	parentEngine := NewEngine(
		cfg,
		cl,
		&State{},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		false,
	)

	parent, err := parentEngine.resolveRemoteSubfolder(ctx, cfg.DriveSubfolder)
	if err != nil {
		t.Fatalf("refusing to continue: expected iCloud test folder not found: %v", err)
	}
	if parent.DrivewsID == "" || parent.DocwsID == "" {
		t.Fatal("refusing to continue: test folder has no drivewsid or docwsid")
	}
	if parent.Zone == "" {
		parent.Zone = icloud.DefaultZone
	}

	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	name := "icloud-synk-live-replace-" + stamp + ".txt"
	oldContent := "alte Testversion " + stamp + "\n"
	newContent := "neue Testversion " + stamp + "\n"

	parentEngine.putDir("", parent)
	children, err := parentEngine.remoteChildren(ctx, parent)
	if err != nil {
		t.Fatalf("checking test folder before upload: %v", err)
	}
	if findRemoteItem(children, name) != nil {
		t.Fatalf("refusing to continue: generated test name already exists: %s", name)
	}

	// Seed a uniquely named test file. The file is deliberately retained.
	if err := cl.Upload(
		ctx, parent.Zone, parent.DocwsID, name,
		int64(len(oldContent)), time.Now(), strings.NewReader(oldContent),
	); err != nil {
		t.Fatalf("uploading initial test version %q: %v", name, err)
	}

	engine, err := newLiveReplaceEngine(cfg, cl, parent)
	if err != nil {
		t.Fatal(err)
	}
	oldItem, err := engine.findRemoteItemRetry(ctx, parent, name)
	if err != nil {
		t.Fatalf("initial test upload was not confirmed; file retained: %v", err)
	}
	if err := engine.verifyRemoteHash(ctx, *oldItem, hashString(oldContent)); err != nil {
		t.Fatalf("initial test upload content verification failed; file retained: %v", err)
	}

	localPath := filepath.Join(cfg.DriveDir(), name)
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte(newContent), 0o600); err != nil {
		t.Fatal(err)
	}

	state, err := LoadState(cfg.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	engine = NewEngine(
		cfg, cl, state,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		false,
	)
	engine.putDir("", parent)

	n, err := engine.upload(ctx, Op{
		Kind:   OpUpload,
		Path:   name,
		Remote: *oldItem,
	}, "live-"+stamp)
	if err != nil {
		t.Fatalf("Engine.upload failed; remote test versions have been retained where possible: %v", err)
	}
	if n != int64(len(newContent)) {
		t.Fatalf("uploaded %d bytes; expected %d", n, len(newContent))
	}

	target, err := engine.findRemoteItemRetry(ctx, parent, name)
	if err != nil {
		t.Fatalf("replacement missing at original name %q: %v", name, err)
	}
	if err := engine.verifyRemoteHash(ctx, *target, hashString(newContent)); err != nil {
		t.Fatalf("final content verification failed; versions retained: %v", err)
	}

	children, err = engine.remoteChildren(ctx, parent)
	if err != nil {
		t.Fatalf("could not confirm backup; no files deleted: %v", err)
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	backupFound := false
	for _, item := range children {
		if item.DrivewsID != target.DrivewsID &&
			strings.HasPrefix(item.FullName(), stem) &&
			strings.Contains(item.FullName(), "backup") {
			backupFound = true
			break
		}
	}
	if !backupFound {
		t.Fatalf("new version verified, but backup not found; no files deleted")
	}

	t.Logf("SUCCESS: Engine.upload replaced %q and verified its content.", name)
	t.Log("The old version remains as a backup. No remote files were deleted.")
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func newLiveReplaceEngine(
	cfg *config.Config,
	cl *icloud.Client,
	parent icloud.Item,
) (*Engine, error) {
	state, err := LoadState(cfg.StatePath())
	if err != nil {
		return nil, fmt.Errorf("loading isolated test state: %w", err)
	}

	engine := NewEngine(
		cfg,
		cl,
		state,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		false,
	)
	engine.putDir("", parent)
	return engine, nil
}
