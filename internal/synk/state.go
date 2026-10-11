// Package synk holds the synchronisation engine: local scanning, three-way
// reconciliation against iCloud, and the daemon loop that drives both.
package synk

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const stateVersion = 1

// Entry records what we knew about a path after the last successful pass. It is
// the third input of the reconciliation: without it, a missing file is
// indistinguishable from a deleted one.
type Entry struct {
	IsDir bool `json:"dir,omitempty"`

	// Local facts, as written or observed by us.
	Size       int64     `json:"size,omitempty"`
	LocalMTime time.Time `json:"local_mtime,omitzero"`
	// SHA256 of the contents as last transferred; lets a touched-but-unchanged
	// file be recognised without re-uploading it.
	SHA256 string `json:"sha256,omitempty"`

	// Remote facts.
	Etag        string    `json:"etag,omitempty"`
	DrivewsID   string    `json:"drivewsid,omitempty"`
	DocwsID     string    `json:"docwsid,omitempty"`
	Zone        string    `json:"zone,omitempty"`
	RemoteMTime time.Time `json:"remote_mtime,omitzero"`
}

// PhotoEntry records a downloaded photo. Keeping it means a photo the user
// deletes locally is not downloaded again on the next pass.
type PhotoEntry struct {
	Path     string    `json:"path"`
	Size     int64     `json:"size,omitempty"`
	Checksum string    `json:"checksum,omitempty"`
	Created  time.Time `json:"created,omitzero"`
}

// State is the on-disk sync database. All access is mutex-guarded because the
// worker pool updates it concurrently.
type State struct {
	mu       sync.Mutex
	saveMu   sync.Mutex
	path     string
	dirty    bool
	revision uint64
	data     stateFile
}

type stateFile struct {
	Version  int                    `json:"version"`
	Updated  time.Time              `json:"updated,omitzero"`
	Drive    map[string]*Entry      `json:"drive"`
	Photos   map[string]*PhotoEntry `json:"photos"`
	SyncedAt time.Time              `json:"synced_at,omitzero"`
}

// LoadState reads the state file, tolerating a missing one.
func LoadState(path string) (*State, error) {
	s := &State{
		path: path,
		data: stateFile{
			Version: stateVersion,
			Drive:   map[string]*Entry{},
			Photos:  map[string]*PhotoEntry{},
		},
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}

	var f stateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("load sync state %s: invalid JSON: %w", path, err)
	}

	if f.Drive == nil {
		f.Drive = map[string]*Entry{}
	}
	if f.Version != stateVersion {
		return nil, fmt.Errorf(
			"load sync state %s: unsupported state version %d (supported: %d)",
			path, f.Version, stateVersion,
		)
	}
	if f.Drive == nil {
		f.Drive = map[string]*Entry{}
	}
	if f.Photos == nil {
		f.Photos = map[string]*PhotoEntry{}
	}
	s.data = f
	return s, nil
}

// Drive returns a snapshot of the Drive entries, safe to read without locks.
func (s *State) Drive() map[string]Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Entry, len(s.data.Drive))
	for k, v := range s.data.Drive {
		out[k] = *v
	}
	return out
}

// PutDrive records the post-operation facts for a path.
func (s *State) PutDrive(path string, e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := e
	s.data.Drive[path] = &cp
	s.dirty = true
	s.revision++
}

// DeleteDrive forgets a path.
func (s *State) DeleteDrive(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Drive, path)
	s.dirty = true
	s.revision++
}

// Photo returns the record of a downloaded photo.
func (s *State) Photo(id string) (PhotoEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data.Photos[id]
	if !ok {
		return PhotoEntry{}, false
	}
	return *e, true
}

// PutPhoto records a downloaded photo.
func (s *State) PutPhoto(id string, e PhotoEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := e
	s.data.Photos[id] = &cp
	s.dirty = true
	s.revision++
}

// PhotoPaths returns the set of local paths already used by photos, so a name
// collision between two different assets can be resolved.
func (s *State) PhotoPaths() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.data.Photos))
	for id, e := range s.data.Photos {
		out[e.Path] = id
	}
	return out
}

// Counts reports how many entries are tracked.
func (s *State) Counts() (drive, photos int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data.Drive), len(s.data.Photos)
}

// SyncedAt reports the end of the last successful pass.
func (s *State) SyncedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.SyncedAt
}

// MarkSynced stamps a successful pass.
func (s *State) MarkSynced(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.SyncedAt = t
	s.dirty = true
	s.revision++
}

// Save atomically writes the state if anything changed.
func (s *State) Save() error {
	// Serialize save operations so an older snapshot cannot overwrite a newer one.
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}

	revision := s.revision
	s.data.Updated = time.Now()
	raw, err := json.Marshal(&s.data)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	f, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}

	s.mu.Lock()
	if s.revision == revision {
		s.dirty = false
	}
	s.mu.Unlock()

	return nil
}
