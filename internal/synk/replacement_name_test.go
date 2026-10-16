package synk

import "testing"

func TestIsReplacementSiblingName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"bericht.icloud-synk-new-20261014T140000.123456789.txt", true},
		{"bericht.icloud-synk-backup-20261014T140000.123456789.txt", true},
		{"bericht.icloud-synk-failed-20261014T140000.123456789.txt", true},
		{"ordner/bericht.icloud-synk-new-20261014T140000.123456789.txt", true},
		{"README.icloud-synk-backup-20261014T140000.123456789", true},
		{"bericht.icloud-synk-notizen.txt", false},
		{"bericht.icloud-synk-new-20261014T140000.txt", false},
		{"bericht.icloud-synk-new-20261314T140000.123456789.txt", false},
		{"bericht.txt", false},
		{"bericht.icloud-synk-new-20261014T140000.123456789extra.txt", false},
		{".icloud-synk-new-20261014T140000.123456789.env", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReplacementSiblingName(tt.name); got != tt.want {
				t.Errorf("isReplacementSiblingName(%q) = %v, want %v",
					tt.name, got, tt.want)
			}
		})
	}
}
func TestReplacementSiblingNameIsRecognized(t *testing.T) {
	stamp := "20261014T140000.123456789"
	names := []string{
		".env",
		".config",
		"README",
		"bericht.txt",
	}

	kinds := []string{"new", "backup", "failed"}

	for _, name := range names {
		for _, kind := range kinds {
			t.Run(name+"/"+kind, func(t *testing.T) {
				got := replacementSiblingName(name, kind, stamp)
				if !isReplacementSiblingName(got) {
					t.Errorf(
						"generated replacement name %q was not recognized",
						got,
					)
				}
			})
		}
	}
}
