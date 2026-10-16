package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestSyncStatusFromJournal(t *testing.T) {
	now := time.Now()
	event := func(age time.Duration, message string) string {
		return now.Add(-age).Format(time.RFC3339) + " host service " + message
	}

	cases := []struct {
		name string
		log  string
		want string
	}{
		{
			"running",
			event(10*time.Second, `level=INFO msg="drive pass started"`),
			"Läuft",
		},
		{
			"stale start",
			event(3*time.Minute, `level=INFO msg="drive pass started"`),
			"Unbekannt",
		},
		{
			"waiting",
			event(10*time.Second, `level=INFO msg="drive pass started"`) + "\n" +
				event(5*time.Second, `level=INFO msg="drive pass completed"`),
			"Wartet auf nächsten Durchlauf",
		},
		{
			"failed",
			event(10*time.Second, `level=INFO msg="drive pass started"`) + "\n" +
				event(5*time.Second, `level=ERROR msg="drive pass failed"`),
			"Letzter Durchlauf fehlgeschlagen",
		},
		{
			"skipped",
			event(5*time.Second, `level=INFO msg="drive pass skipped"`),
			"Durchlauf übersprungen",
		},
		{
			"unknown",
			"daemon started\nother message",
			"Unbekannt",
		},
		{
			"latest event wins",
			event(10*time.Second, `level=INFO msg="drive pass started"`) + "\n" +
				event(5*time.Second, `level=INFO msg="drive pass completed"`),
			"Wartet auf nächsten Durchlauf",
		},
		{
			"failure followed by successful completion",
			event(10*time.Second, `level=INFO msg="drive pass started"`) + "\n" +
				event(8*time.Second, `level=ERROR msg="drive pass failed"`) + "\n" +
				event(5*time.Second, `level=INFO msg="drive pass started"`) + "\n" +
				event(2*time.Second, `level=INFO msg="drive pass completed"`),
			"Wartet auf nächsten Durchlauf",
		},
		{
			"success followed by newer failure",
			event(10*time.Second, `level=INFO msg="drive pass started"`) + "\n" +
				event(8*time.Second, `level=INFO msg="drive pass completed"`) + "\n" +
				event(5*time.Second, `level=INFO msg="drive pass started"`) + "\n" +
				event(2*time.Second, `level=ERROR msg="drive pass failed"`),
			"Letzter Durchlauf fehlgeschlagen",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := syncStatusFromJournal(tc.log); got != tc.want {
				t.Fatalf("syncStatusFromJournal() = %q, want %q", got, tc.want)
			}
		})
	}
}
