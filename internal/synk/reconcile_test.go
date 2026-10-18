package synk

import (
	"testing"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

var basePolicy = Policy{DeleteLocal: true, DeleteRemote: true, Conflict: "both"}

func file(p string, size int64, mtime time.Time) LocalFile {
	return LocalFile{Path: p, Size: size, MTime: mtime}
}

func remoteFile(name, etag string, size int64, mtime time.Time) icloud.Item {
	return icloud.Item{
		DrivewsID:    "FILE::com.apple.CloudDocs::" + name,
		DocwsID:      name,
		Zone:         icloud.DefaultZone,
		Name:         name,
		Etag:         etag,
		Type:         icloud.TypeFile,
		Size:         size,
		DateModified: mtime,
	}
}

// findOp returns the single operation for a path.
func findOp(t *testing.T, ops []Op, path string) Op {
	t.Helper()
	var found []Op
	for _, op := range ops {
		if op.Path == path {
			found = append(found, op)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one op for %q, got %d: %+v", path, len(found), found)
	}
	return found[0]
}

func TestReconcileNewFiles(t *testing.T) {
	now := time.Now()
	local := map[string]LocalFile{"a.txt": file("a.txt", 10, now)}
	remote := map[string]icloud.Item{"b.txt": remoteFile("b.txt", "e1", 20, now)}

	ops := Reconcile(local, remote, map[string]Entry{}, basePolicy)

	if got := findOp(t, ops, "a.txt"); got.Kind != OpUpload {
		t.Errorf("new local file: got %s, want %s", got.Kind, OpUpload)
	}
	if got := findOp(t, ops, "b.txt"); got.Kind != OpDownload {
		t.Errorf("new remote file: got %s, want %s", got.Kind, OpDownload)
	}
}

func TestReconcileSkipsInternalReplacementFiles(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	names := []string{
		"report.icloud-synk-new-20261014T140000.123456789.txt",
		"report.icloud-synk-backup-20261014T140000.123456789.txt",
		"report.icloud-synk-failed-20261014T140000.123456789.txt",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Run("remote only", func(t *testing.T) {
				ops := Reconcile(
					map[string]LocalFile{},
					map[string]icloud.Item{name: remoteFile(name, "e1", 10, now)},
					map[string]Entry{},
					basePolicy,
				)
				if got := findOp(t, ops, name); got.Kind != OpSkip {
					t.Fatalf("got %s, want %s", got.Kind, OpSkip)
				}
			})

			t.Run("local only", func(t *testing.T) {
				ops := Reconcile(
					map[string]LocalFile{name: file(name, 10, now)},
					map[string]icloud.Item{},
					map[string]Entry{},
					basePolicy,
				)
				if got := findOp(t, ops, name); got.Kind != OpSkip {
					t.Fatalf("got %s, want %s", got.Kind, OpSkip)
				}
			})

			t.Run("both sides", func(t *testing.T) {
				ops := Reconcile(
					map[string]LocalFile{name: file(name, 10, now)},
					map[string]icloud.Item{name: remoteFile(name, "e1", 10, now)},
					map[string]Entry{},
					basePolicy,
				)
				if got := findOp(t, ops, name); got.Kind != OpSkip {
					t.Fatalf("got %s, want %s", got.Kind, OpSkip)
				}
			})

			t.Run("state only", func(t *testing.T) {
				state := map[string]Entry{
					name: {Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "remote-id"},
				}
				ops := Reconcile(
					map[string]LocalFile{},
					map[string]icloud.Item{},
					state,
					basePolicy,
				)
				if got := findOp(t, ops, name); got.Kind != OpSkip {
					t.Fatalf("got %s, want %s", got.Kind, OpSkip)
				}
			})
		})
	}
}

func TestReconcileUnchangedFileDoesNothing(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	local := map[string]LocalFile{"a.txt": file("a.txt", 10, now)}
	remote := map[string]icloud.Item{"a.txt": remoteFile("a.txt", "e1", 10, now)}
	state := map[string]Entry{"a.txt": {Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "x"}}

	ops := Reconcile(local, remote, state, basePolicy)

	if got := findOp(t, ops, "a.txt"); got.Kind != OpRecord {
		t.Errorf("unchanged file: got %s, want %s", got.Kind, OpRecord)
	}
}

func TestReconcileLocalEditUploads(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	now := time.Now().Truncate(time.Second)
	local := map[string]LocalFile{"a.txt": file("a.txt", 99, now)}
	remote := map[string]icloud.Item{"a.txt": remoteFile("a.txt", "e1", 10, old)}
	state := map[string]Entry{"a.txt": {Size: 10, LocalMTime: old, Etag: "e1", DrivewsID: "x"}}

	ops := Reconcile(local, remote, state, basePolicy)

	op := findOp(t, ops, "a.txt")
	if op.Kind != OpUpload || op.Conflict {
		t.Errorf("local edit: got %s (conflict=%v), want %s without conflict", op.Kind, op.Conflict, OpUpload)
	}
}

func TestReconcileRemoteEditDownloads(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	local := map[string]LocalFile{"a.txt": file("a.txt", 10, old)}
	remote := map[string]icloud.Item{"a.txt": remoteFile("a.txt", "e2", 42, time.Now())}
	state := map[string]Entry{"a.txt": {Size: 10, LocalMTime: old, Etag: "e1", DrivewsID: "x"}}

	ops := Reconcile(local, remote, state, basePolicy)

	op := findOp(t, ops, "a.txt")
	if op.Kind != OpDownload || op.Conflict {
		t.Errorf("remote edit: got %s (conflict=%v), want %s without conflict", op.Kind, op.Conflict, OpDownload)
	}
}

func TestReconcileConflictNewerWins(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	localNewer := time.Now().Truncate(time.Second)
	remoteMid := time.Now().Add(-time.Minute)

	state := map[string]Entry{"a.txt": {Size: 10, LocalMTime: old, Etag: "e1", DrivewsID: "x"}}
	local := map[string]LocalFile{"a.txt": file("a.txt", 11, localNewer)}
	remote := map[string]icloud.Item{"a.txt": remoteFile("a.txt", "e2", 12, remoteMid)}

	op := findOp(t, Reconcile(local, remote, state, basePolicy), "a.txt")
	if !op.Conflict || op.Kind != OpUpload || op.Winner != WinnerLocal {
		t.Errorf("local-newer conflict: got %s winner=%s conflict=%v", op.Kind, op.Winner, op.Conflict)
	}

	// Now make the remote side the newer one.
	remote["a.txt"] = remoteFile("a.txt", "e2", 12, localNewer.Add(time.Minute))
	op = findOp(t, Reconcile(local, remote, state, basePolicy), "a.txt")
	if !op.Conflict || op.Kind != OpDownload || op.Winner != WinnerRemote {
		t.Errorf("remote-newer conflict: got %s winner=%s conflict=%v", op.Kind, op.Winner, op.Conflict)
	}
}

func TestReconcileConflictWhenRemoteSizeChangesAndETagDoesNot(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	localNewer := time.Now().Truncate(time.Second)

	state := map[string]Entry{
		"a.txt": {Size: 10, LocalMTime: old, Etag: "e1", DrivewsID: "x"},
	}
	local := map[string]LocalFile{
		"a.txt": file("a.txt", 11, localNewer),
	}
	remote := map[string]icloud.Item{
		"a.txt": remoteFile("a.txt", "e1", 12, old),
	}

	op := findOp(t, Reconcile(local, remote, state, basePolicy), "a.txt")
	if !op.Conflict || op.Kind != OpUpload || op.Winner != WinnerLocal {
		t.Errorf(
			"size change with unchanged ETag and newer local file: got %s winner=%s conflict=%v",
			op.Kind, op.Winner, op.Conflict,
		)
	}
}

func TestReconcileConflictEqualTimestampsPreferRemote(t *testing.T) {
	sameTime := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	state := map[string]Entry{
		"a.txt": {Size: 10, LocalMTime: sameTime.Add(-time.Hour), Etag: "e1", DrivewsID: "x"},
	}
	local := map[string]LocalFile{
		"a.txt": file("a.txt", 11, sameTime),
	}
	remote := map[string]icloud.Item{
		"a.txt": remoteFile("a.txt", "e2", 12, sameTime),
	}

	op := findOp(t, Reconcile(local, remote, state, basePolicy), "a.txt")

	if !op.Conflict {
		t.Fatal("expected a conflict")
	}
	if op.Kind != OpDownload || op.Winner != WinnerRemote {
		t.Errorf("equal timestamps: got %s winner=%s, want download/remote", op.Kind, op.Winner)
	}
}

func TestReconcileConflictRemotePolicyAlwaysDownloads(t *testing.T) {
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	state := map[string]Entry{"a.txt": {Size: 10, LocalMTime: old, Etag: "e1", DrivewsID: "x"}}
	local := map[string]LocalFile{"a.txt": file("a.txt", 11, time.Now())}
	remote := map[string]icloud.Item{"a.txt": remoteFile("a.txt", "e2", 12, old)}

	p := basePolicy
	p.Conflict = "remote"
	op := findOp(t, Reconcile(local, remote, state, p), "a.txt")
	if op.Kind != OpDownload || op.Winner != WinnerRemote {
		t.Errorf("conflict=remote: got %s winner=%s, want download/remote", op.Kind, op.Winner)
	}
}

func TestReconcileDeletions(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	state := map[string]Entry{
		"gone-remote.txt": {Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "x"},
		"gone-local.txt":  {Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "y"},
	}
	local := map[string]LocalFile{"gone-remote.txt": file("gone-remote.txt", 10, now)}
	remote := map[string]icloud.Item{"gone-local.txt": remoteFile("gone-local.txt", "e1", 10, now)}

	ops := Reconcile(local, remote, state, basePolicy)

	if got := findOp(t, ops, "gone-remote.txt"); got.Kind != OpTrashLocal {
		t.Errorf("file removed in iCloud: got %s, want %s", got.Kind, OpTrashLocal)
	}
	if got := findOp(t, ops, "gone-local.txt"); got.Kind != OpTrashRemote {
		t.Errorf("file removed locally: got %s, want %s", got.Kind, OpTrashRemote)
	}
}

func TestReconcileProtectsAgainstDeletionActions(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	state := map[string]Entry{
		"gone-remote.txt": {Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "x"},
		"gone-local.txt":  {Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "y"},
	}
	local := map[string]LocalFile{
		"gone-remote.txt": file("gone-remote.txt", 10, now),
	}
	remote := map[string]icloud.Item{
		"gone-local.txt": remoteFile("gone-local.txt", "e1", 10, now),
	}
	policy := Policy{
		DeleteLocal: true, DeleteRemote: true,
		ProtectDeletions: true, Conflict: "both",
	}

	ops := Reconcile(local, remote, state, policy)
	for _, op := range ops {
		if op.Kind == OpTrashLocal || op.Kind == OpTrashRemote {
			t.Errorf("deletion action %s planned for %q despite protection", op.Kind, op.Path)
		}
	}
	if got := findOp(t, ops, "gone-remote.txt"); got.Kind != OpSkip {
		t.Errorf("missing iCloud file: got %s, want %s", got.Kind, OpSkip)
	}
	if got := findOp(t, ops, "gone-local.txt"); got.Kind != OpSkip {
		t.Errorf("missing local file: got %s, want %s", got.Kind, OpSkip)
	}
}

func TestReconcileDeletionsDisabledResurrects(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	state := map[string]Entry{
		"gone-remote.txt": {Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "x"},
		"gone-local.txt":  {Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "y"},
	}
	local := map[string]LocalFile{"gone-remote.txt": file("gone-remote.txt", 10, now)}
	remote := map[string]icloud.Item{"gone-local.txt": remoteFile("gone-local.txt", "e1", 10, now)}

	p := Policy{DeleteLocal: false, DeleteRemote: false, Conflict: "both"}
	ops := Reconcile(local, remote, state, p)

	if got := findOp(t, ops, "gone-remote.txt"); got.Kind != OpUpload {
		t.Errorf("deletions off: got %s, want %s", got.Kind, OpUpload)
	}
	if got := findOp(t, ops, "gone-local.txt"); got.Kind != OpDownload {
		t.Errorf("deletions off: got %s, want %s", got.Kind, OpDownload)
	}
}

func TestReconcileDirectories(t *testing.T) {
	dir := icloud.Item{
		DrivewsID: "FOLDER::com.apple.CloudDocs::docs",
		DocwsID:   "docs",
		Name:      "docs",
		Type:      icloud.TypeFolder,
		Etag:      "d1",
	}
	local := map[string]LocalFile{"newdir": {Path: "newdir", IsDir: true}}
	remote := map[string]icloud.Item{"docs": dir}

	ops := Reconcile(local, remote, map[string]Entry{}, basePolicy)

	if got := findOp(t, ops, "docs"); got.Kind != OpMkdirLocal {
		t.Errorf("remote-only folder: got %s, want %s", got.Kind, OpMkdirLocal)
	}
	if got := findOp(t, ops, "newdir"); got.Kind != OpMkdirRemote {
		t.Errorf("local-only folder: got %s, want %s", got.Kind, OpMkdirRemote)
	}
	// Directories must be created before anything inside them.
	for i, op := range ops {
		if op.Kind == OpDownload || op.Kind == OpUpload {
			for _, later := range ops[i:] {
				if later.Kind == OpMkdirLocal || later.Kind == OpMkdirRemote {
					t.Fatalf("directory creation sorted after a transfer: %+v", ops)
				}
			}
		}
	}
}

func TestReconcileForgetsVanishedPaths(t *testing.T) {
	state := map[string]Entry{"old.txt": {Size: 1, LocalMTime: time.Now(), Etag: "e"}}
	ops := Reconcile(map[string]LocalFile{}, map[string]icloud.Item{}, state, basePolicy)
	if got := findOp(t, ops, "old.txt"); got.Kind != OpForget {
		t.Errorf("vanished path: got %s, want %s", got.Kind, OpForget)
	}
}

func TestReconcileProtectsVanishedPaths(t *testing.T) {
	state := map[string]Entry{
		"old.txt": {
			Size: 1, LocalMTime: time.Now(), Etag: "e",
			DrivewsID: "remote-id",
		},
	}
	policy := Policy{Conflict: "both", ProtectDeletions: true}

	ops := Reconcile(
		map[string]LocalFile{},
		map[string]icloud.Item{},
		state,
		policy,
	)
	if got := findOp(t, ops, "old.txt"); got.Kind != OpForget {
		t.Errorf("vanished path: got %s, want %s", got.Kind, OpForget)
	}
}

func TestReconcileSkipsPackages(t *testing.T) {
	pkg := icloud.Item{
		DrivewsID: "FILE::com.apple.Pages::doc",
		Name:      "Report", Extension: "pages",
		Type: icloud.TypeFile, ItemCount: 12, Etag: "p1",
	}
	ops := Reconcile(map[string]LocalFile{}, map[string]icloud.Item{"Report.pages": pkg}, map[string]Entry{}, basePolicy)
	if got := findOp(t, ops, "Report.pages"); got.Kind != OpSkip {
		t.Errorf("package: got %s, want %s", got.Kind, OpSkip)
	}
}

func TestSortOpsDeletesDeepestFirst(t *testing.T) {
	ops := []Op{
		{Kind: OpTrashLocal, Path: "a"},
		{Kind: OpTrashLocal, Path: "a/b/c"},
		{Kind: OpTrashLocal, Path: "a/b"},
	}
	sortOps(ops)
	want := []string{"a/b/c", "a/b", "a"}
	for i, w := range want {
		if ops[i].Path != w {
			t.Fatalf("deletion order = %v, want %v", []Op{ops[0], ops[1], ops[2]}, want)
		}
	}
}

func TestReconcileProtectsPreviouslySyncedMissingFiles(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	policy := Policy{Conflict: "both", ProtectDeletions: true}

	t.Run("local file missing", func(t *testing.T) {
		remote := map[string]icloud.Item{"a.txt": remoteFile("a.txt", "e1", 10, now)}
		state := map[string]Entry{"a.txt": {Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "remote-id"}}
		op := findOp(t, Reconcile(map[string]LocalFile{}, remote, state, policy), "a.txt")
		if op.Kind != OpSkip {
			t.Fatalf("got %s, want %s", op.Kind, OpSkip)
		}
	})

	t.Run("remote file missing", func(t *testing.T) {
		local := map[string]LocalFile{"a.txt": file("a.txt", 10, now)}
		state := map[string]Entry{"a.txt": {Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "remote-id"}}
		op := findOp(t, Reconcile(local, map[string]icloud.Item{}, state, policy), "a.txt")
		if op.Kind != OpSkip {
			t.Fatalf("got %s, want %s", op.Kind, OpSkip)
		}
	})
}

func TestReconcileProtectsPreviouslySyncedMissingDirectories(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	policy := Policy{Conflict: "both", ProtectDeletions: true}

	t.Run("local directory missing", func(t *testing.T) {
		remote := map[string]icloud.Item{"folder": {DrivewsID: "dir-id", Name: "folder", Type: icloud.TypeFolder}}
		state := map[string]Entry{"folder": {IsDir: true, DrivewsID: "dir-id", LocalMTime: now}}
		op := findOp(t, Reconcile(map[string]LocalFile{}, remote, state, policy), "folder")
		if op.Kind != OpSkip {
			t.Fatalf("got %s, want %s", op.Kind, OpSkip)
		}
	})

	t.Run("remote directory missing", func(t *testing.T) {
		local := map[string]LocalFile{"folder": {Path: "folder", IsDir: true, MTime: now}}
		state := map[string]Entry{"folder": {IsDir: true, DrivewsID: "dir-id", LocalMTime: now}}
		op := findOp(t, Reconcile(local, map[string]icloud.Item{}, state, policy), "folder")
		if op.Kind != OpSkip {
			t.Fatalf("got %s, want %s", op.Kind, OpSkip)
		}
	})
}

func TestReconcileProtectsOversizedLocalFile(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	state := map[string]Entry{
		"large.bin": {
			Size: 100, LocalMTime: now, Etag: "e1",
			DrivewsID: "remote-id",
		},
	}
	remote := map[string]icloud.Item{
		"large.bin": remoteFile("large.bin", "e1", 100, now),
	}
	policy := Policy{
		DeleteLocal: true, DeleteRemote: true, Conflict: "both",
		ProtectedPaths: map[string]bool{"large.bin": true},
	}

	ops := Reconcile(map[string]LocalFile{}, remote, state, policy)
	op := findOp(t, ops, "large.bin")
	if op.Kind != OpSkip {
		t.Fatalf("got %s, want %s", op.Kind, OpSkip)
	}
	for _, op := range ops {
		if op.Kind == OpTrashLocal || op.Kind == OpTrashRemote ||
			op.Kind == OpDownload || op.Kind == OpUpload {
			t.Fatalf("protected path has unsafe operation: %+v", op)
		}
	}

	// Even if the path is missing on both sides, keep its state entry
	// while the local scanner has explicitly protected it.
	ops = Reconcile(map[string]LocalFile{}, map[string]icloud.Item{}, state, policy)
	op = findOp(t, ops, "large.bin")
	if op.Kind != OpSkip {
		t.Fatalf("when missing on both sides, got %s, want %s", op.Kind, OpSkip)
	}
}

func TestReconcileRemoteSizeChangeDownloadsWhenETagUnchanged(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	local := map[string]LocalFile{
		"a.txt": file("a.txt", 10, now),
	}
	remote := map[string]icloud.Item{
		"a.txt": remoteFile("a.txt", "e1", 52, now),
	}
	state := map[string]Entry{
		"a.txt": {
			Size: 10, LocalMTime: now, Etag: "e1", DrivewsID: "x",
		},
	}

	ops := Reconcile(local, remote, state, basePolicy)
	op := findOp(t, ops, "a.txt")

	if op.Kind != OpDownload || op.Conflict {
		t.Errorf(
			"remote size change with unchanged ETag: got %s (conflict=%v), want %s without conflict",
			op.Kind, op.Conflict, OpDownload,
		)
	}
}

func TestReconcileUnchangedRemoteMetadataRecordsState(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	local := map[string]LocalFile{
		"a.txt": file("a.txt", 10, now),
	}
	remote := map[string]icloud.Item{
		"a.txt": remoteFile("a.txt", "e1", 10, now),
	}
	state := map[string]Entry{
		"a.txt": {
			Size: 10, LocalMTime: now, Etag: "e1",
			RemoteMTime: now, DrivewsID: "x",
		},
	}

	ops := Reconcile(local, remote, state, basePolicy)
	op := findOp(t, ops, "a.txt")
	if op.Kind != OpRecord {
		t.Fatalf("unchanged remote metadata: got %s, want %s", op.Kind, OpRecord)
	}
}

func TestReconcileRemoteMTimeChangeWithUnchangedETagAndSizeRecordsState(t *testing.T) {
	old := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	changed := old.Add(time.Hour)

	local := map[string]LocalFile{
		"a.txt": file("a.txt", 10, old),
	}
	remote := map[string]icloud.Item{
		"a.txt": remoteFile("a.txt", "e1", 10, changed),
	}
	state := map[string]Entry{
		"a.txt": {
			Size: 10, LocalMTime: old, Etag: "e1",
			RemoteMTime: old, DrivewsID: "x",
		},
	}

	ops := Reconcile(local, remote, state, basePolicy)
	op := findOp(t, ops, "a.txt")
	// A metadata-only change does not trigger a download; Reconcile records state.
	if op.Kind != OpRecord {
		t.Fatalf("mtime-only change with unchanged ETag and size: got %s, want %s", op.Kind, OpRecord)
	}
}
