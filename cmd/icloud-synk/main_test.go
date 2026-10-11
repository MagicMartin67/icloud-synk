package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kolesnikav/icloud-synk/internal/synk"
)

func TestFinishSyncDryRunDoesNotSaveState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	st, err := synk.LoadState(statePath)
	if err != nil {
		t.Fatal(err)
	}

	if err := finishSync(st, true); err != nil {
		t.Fatalf("finishSync dry run failed: %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("dry run unexpectedly wrote state file; stat error: %v", err)
	}
}
