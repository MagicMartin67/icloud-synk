package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsProtectDeletions(t *testing.T) {
	cfg := Defaults()
	if !cfg.ProtectDeletions {
		t.Fatal("ProtectDeletions must be enabled by default")
	}
}

func TestValidateDriveSubfolder(t *testing.T) {
	valid := []string{"", "Tappe/Dokumente sync NAS", "Ordner"}
	for _, subfolder := range valid {
		t.Run(subfolder, func(t *testing.T) {
			cfg := Defaults()
			cfg.TargetDir = t.TempDir()
			cfg.DriveSubfolder = subfolder
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() rejected %q: %v", subfolder, err)
			}
		})
	}
}

func TestValidateRejectsUnsafeDriveSubfolder(t *testing.T) {
	invalid := []string{"/Tappe/Dokumente", "../Dokumente", "Tappe/../Dokumente", "Tappe//Dokumente", "Tappe\\Dokumente"}
	for _, subfolder := range invalid {
		t.Run(subfolder, func(t *testing.T) {
			cfg := Defaults()
			cfg.TargetDir = t.TempDir()
			cfg.DriveSubfolder = subfolder
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() accepted unsafe path %q", subfolder)
			}
		})
	}
}
func TestLoadRespectsExplicitProtectDeletionsFalse(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)

	configDir := filepath.Join(configHome, "icloud-synk")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}

	configFile := filepath.Join(configDir, "config.json")
	content := `{"target_dir":"/tmp/icloud-synk-test","protect_deletions":false}`
	if err := os.WriteFile(configFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, ok, err := Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if !ok {
		t.Fatal("Load() reported that the configuration was not found")
	}
	if cfg.ProtectDeletions {
		t.Fatal("explicit protect_deletions=false should override the default")
	}
}
