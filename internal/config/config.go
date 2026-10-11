// Package config loads and stores the user-visible configuration of
// icloud-synk (~/.config/icloud-synk/config.json).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the on-disk configuration. Every field has a usable default so an
// empty file still yields a working setup once target_dir is filled in.
type Config struct {
	AppleID string `json:"apple_id"`

	// TargetDir is the local root. Drive/ and Photos/ are created inside it.
	TargetDir string `json:"target_dir"`

	// DriveSubfolder limits Drive sync to a relative path inside iCloud Drive.
	// Empty means the entire iCloud Drive (the original behaviour).
	DriveSubfolder string `json:"drive_subfolder"`

	// ProtectDeletions leaves one-sided absences untouched when the path was synced before.
	ProtectDeletions bool `json:"protect_deletions"`

	SyncDrive  bool `json:"sync_drive"`
	SyncPhotos bool `json:"sync_photos"`

	// PollInterval controls how often the cloud side is polled for changes.
	// Apple's web API has no push channel, so this is the remote-side latency.
	PollInterval Duration `json:"poll_interval"`
	// PhotosInterval is polled separately: the photo library is expensive to
	// enumerate and changes less often than Drive.
	PhotosInterval Duration `json:"photos_interval"`
	// Debounce is how long a locally changed file must be quiet before upload.
	Debounce Duration `json:"debounce"`

	Workers int `json:"workers"`

	// DeleteRemote propagates local deletions to iCloud's trash.
	DeleteRemote bool `json:"delete_remote"`
	// DeleteLocal moves locally present files to .icloud-synk/trash when they
	// disappear from iCloud.
	DeleteLocal bool `json:"delete_local"`

	// Conflict is one of "both" (keep both versions), "newer", "remote".
	Conflict string `json:"conflict"`

	// MaxFileSize skips files larger than this many bytes (0 = no limit).
	MaxFileSize int64 `json:"max_file_size"`

	// Exclude is a list of shell patterns matched against each path segment.
	Exclude []string `json:"exclude"`

	// Notify sends desktop notifications (notify-send) for re-auth prompts.
	Notify bool `json:"notify"`

	// PasswordCommand, when set, is run to obtain the Apple ID password for
	// unattended re-login (e.g. "secret-tool lookup service icloud").
	PasswordCommand string `json:"password_command"`
}

// Duration is a time.Duration that marshals as a human string ("30s").
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n int64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return err
		}
		*d = Duration(time.Duration(n) * time.Second)
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// Defaults returns a Config with every non-path field populated.
func Defaults() *Config {
	return &Config{
		SyncDrive:        true,
		SyncPhotos:       true,
		PollInterval:     Duration(30 * time.Second),
		PhotosInterval:   Duration(15 * time.Minute),
		Debounce:         Duration(2 * time.Second),
		Workers:          4,
		DeleteRemote:     true,
		DeleteLocal:      true,
		ProtectDeletions: true,
		Conflict:         "both",
		Notify:           true,
		Exclude: []string{
			".icloud-synk", ".DS_Store", "*.part", "*.crdownload", "*.partial",
			".goutputstream-*", "~$*", ".~lock.*", "*.swp", "4913", ".Trash-*",
		},
	}
}

// Dir is the configuration directory (~/.config/icloud-synk).
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "icloud-synk"), nil
}

// Path is the path of config.json.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads config.json, filling in defaults for absent fields. A missing
// file is not an error: the defaults are returned with ok == false.
func Load() (cfg *Config, ok bool, err error) {
	p, err := Path()
	if err != nil {
		return nil, false, err
	}
	cfg = Defaults()
	raw, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return cfg, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, false, fmt.Errorf("%s: %w", p, err)
	}
	return cfg, true, nil
}

// Save writes config.json, creating the directory if needed.
func (c *Config) Save() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dir, "config.json")
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Validate checks the fields that cannot be defaulted and normalises paths.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.TargetDir) == "" {
		return fmt.Errorf("target_dir is not set — run: icloud-synk login --dir /path/to/folder")
	}
	if strings.HasPrefix(c.TargetDir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		c.TargetDir = filepath.Join(home, strings.TrimPrefix(c.TargetDir, "~"))
	}
	abs, err := filepath.Abs(c.TargetDir)
	if err != nil {
		return err
	}
	c.TargetDir = abs
	if c.DriveSubfolder != "" {
		if strings.HasPrefix(c.DriveSubfolder, "/") || strings.Contains(c.DriveSubfolder, "\\") {
			return fmt.Errorf("drive_subfolder must be a relative iCloud Drive path using '/' separators")
		}
		for _, part := range strings.Split(c.DriveSubfolder, "/") {
			if part == "" || part == "." || part == ".." {
				return fmt.Errorf("drive_subfolder contains an invalid path component %q", part)
			}
		}
	}
	if c.Workers < 1 {
		c.Workers = 1
	}
	if c.PollInterval.D() < 5*time.Second {
		c.PollInterval = Duration(5 * time.Second)
	}
	switch c.Conflict {
	case "both", "newer", "remote":
	default:
		return fmt.Errorf("conflict must be one of both|newer|remote, got %q", c.Conflict)
	}
	if !c.SyncDrive && !c.SyncPhotos {
		return fmt.Errorf("both sync_drive and sync_photos are disabled — nothing to do")
	}
	return nil
}

// DriveDir is the local root of the two-way Drive mirror.
func (c *Config) DriveDir() string { return filepath.Join(c.TargetDir, "Drive") }

// PhotosDir is the local root of the (download-only) photo library.
func (c *Config) PhotosDir() string { return filepath.Join(c.TargetDir, "Photos") }

// MetaDir holds state and the local trash.
func (c *Config) MetaDir() string { return filepath.Join(c.TargetDir, ".icloud-synk") }

// TrashDir is where files deleted in iCloud are moved to locally.
func (c *Config) TrashDir() string { return filepath.Join(c.MetaDir(), "trash") }

// StatePath is the sync state database.
func (c *Config) StatePath() string { return filepath.Join(c.MetaDir(), "state.json") }
