package synk

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kolesnikav/icloud-synk/internal/config"
)

func TestScannerFindsFilesAndSkipsExcluded(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "notes.txt"), "hello")
	mustMkdir(t, filepath.Join(root, "sub"))
	mustWrite(t, filepath.Join(root, "sub", "deep.bin"), "xx")
	mustMkdir(t, filepath.Join(root, ".icloud-synk", "trash"))
	mustWrite(t, filepath.Join(root, ".icloud-synk", "trash", "old.txt"), "nope")
	mustWrite(t, filepath.Join(root, "download.part"), "partial")

	s := &Scanner{Root: root, Exclude: config.Defaults().Exclude}
	files, _, _, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for p := range files {
		got = append(got, p)
	}
	slices.Sort(got)
	want := []string{"notes.txt", "sub", "sub/deep.bin"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scan = %v, want %v", got, want)
	}
	if !files["sub"].IsDir {
		t.Error("sub should be reported as a directory")
	}
	if files["notes.txt"].Size != 5 {
		t.Errorf("notes.txt size = %d, want 5", files["notes.txt"].Size)
	}
}

func TestScannerSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.txt")
	mustWrite(t, target, "data")
	if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	s := &Scanner{Root: root}
	files, skipped, _, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["link.txt"]; ok {
		t.Error("symlink was included in the scan")
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "symlink") {
		t.Errorf("skipped = %v, want one symlink note", skipped)
	}
}

func TestScannerMaxSize(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "big.bin"), strings.Repeat("x", 100))
	mustWrite(t, filepath.Join(root, "small.bin"), "x")

	s := &Scanner{Root: root, MaxSize: 10}
	files, skipped, oversized, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files["big.bin"]; ok {
		t.Error("big.bin should have been skipped")
	}
	if _, ok := files["small.bin"]; !ok {
		t.Error("small.bin should have been kept")
	}
	if len(skipped) != 0 {
		t.Errorf("skipped = %v, want no ordinary skipped entries", skipped)
	}
	if len(oversized) != 1 || oversized[0] != "big.bin" {
		t.Errorf("oversized = %v, want [big.bin]", oversized)
	}
}

func TestIsExcludedMatchesAnySegment(t *testing.T) {
	s := &Scanner{Exclude: []string{".icloud-synk", "*.part", "~$*"}}
	cases := map[string]bool{
		"a/b/c.txt":               false,
		".icloud-synk/state.json": true,
		"docs/.icloud-synk/x":     true,
		"docs/report.part":        true,
		"docs/~$report.docx":      true,
		"docs/part":               false,
		"docs/notes.partial.txt":  false,
	}
	for p, want := range cases {
		if got := s.IsExcluded(p); got != want {
			t.Errorf("IsExcluded(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestConflictPathKeepsExtension(t *testing.T) {
	got := conflictPath("/home/ak/iCloud/Drive/report.txt", "2026-08-06T12-00-00")
	want := "/home/ak/iCloud/Drive/report.conflict-2026-08-06T12-00-00.txt"
	if got != want {
		t.Errorf("conflictPath = %q, want %q", got, want)
	}
	if got := conflictPath("/tmp/README", "S"); got != "/tmp/README.conflict-S" {
		t.Errorf("extensionless conflictPath = %q", got)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	st, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	st.PutDrive("a/b.txt", Entry{Size: 7, Etag: "e1", SHA256: "abc"})
	st.PutPhoto("MASTER1", PhotoEntry{Path: "2026/08/IMG_1.HEIC", Size: 11})
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	again, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if e := again.Drive()["a/b.txt"]; e.Size != 7 || e.Etag != "e1" || e.SHA256 != "abc" {
		t.Errorf("drive entry did not survive: %+v", e)
	}
	p, ok := again.Photo("MASTER1")
	if !ok || p.Path != "2026/08/IMG_1.HEIC" {
		t.Errorf("photo entry did not survive: %+v", p)
	}
	if d, ph := again.Counts(); d != 1 || ph != 1 {
		t.Errorf("counts = %d/%d, want 1/1", d, ph)
	}
}

func TestSaveFailureCanBeRetried(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")

	statePath := filepath.Join(root, "state.json")
	st, err := LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}

	st.PutDrive("retry.txt", Entry{
		Size:   7,
		Etag:   "retry-etag",
		SHA256: "retry-hash",
	})

	// Replace the intended parent directory with a regular file.
	if err := os.WriteFile(blocker, []byte("blocker"), 0o600); err != nil {
		t.Fatal(err)
	}
	st.path = filepath.Join(blocker, "state.json")

	if err := st.Save(); err == nil {
		t.Fatal("expected the first save to fail")
	}

	// Restore a valid state path and retry.
	st.path = statePath
	if err := st.Save(); err != nil {
		t.Fatalf("retry save failed: %v", err)
	}

	loaded, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("could not load state after retry: %v", err)
	}

	entry, ok := loaded.Drive()["retry.txt"]
	if !ok {
		t.Fatal("entry was not persisted after retry")
	}
	if entry.Etag != "retry-etag" || entry.SHA256 != "retry-hash" {
		t.Fatalf("unexpected persisted entry: %+v", entry)
	}
}

func TestLoadStateRejectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	mustWrite(t, path, "{not json")

	_, err := LoadState(path)
	if err == nil {
		t.Fatal("expected an error for a corrupted state file")
	}
}

func TestLoadStateRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	mustWrite(t, path, `{
		"version": 999,
		"drive": {},
		"photos": {}
	}`)

	_, err := LoadState(path)
	if err == nil {
		t.Fatal("expected an error for an unsupported state version")
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}
