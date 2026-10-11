package synk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// LocalFile is one entry of the local tree, keyed by a slash-separated path
// relative to the Drive root.
type LocalFile struct {
	Path  string
	IsDir bool
	Size  int64
	MTime time.Time
}

// Scanner walks the local mirror, applying the exclude patterns.
type Scanner struct {
	Root    string
	Exclude []string
	MaxSize int64
}

// Scan returns every file and directory below Root. Symlinks are reported as
// skipped rather than followed: iCloud has no equivalent, and following them
// risks copying half the filesystem into the cloud.
func (s *Scanner) Scan() (files map[string]LocalFile, skipped, oversized []string, err error) {
	files = make(map[string]LocalFile)
	rootFS := s.Root
	err = filepath.WalkDir(rootFS, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An incomplete local inventory must abort the scan. Continuing
			// could make existing files appear deleted and trigger unsafe actions.
			return fmt.Errorf("walking %s: %w", p, walkErr)
		}
		if p == rootFS {
			return nil
		}
		rel := relSlash(rootFS, p)
		if s.isExcluded(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			skipped = append(skipped, rel+" (symlink)")
			return nil
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			skipped = append(skipped, rel+" (not a regular file)")
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return fmt.Errorf("reading file information for %s: %w", p, statErr)
		}
		if !d.IsDir() && s.MaxSize > 0 && info.Size() > s.MaxSize {
			oversized = append(oversized, rel)
			return nil
		}
		files[rel] = LocalFile{
			Path:  rel,
			IsDir: d.IsDir(),
			Size:  info.Size(),
			MTime: info.ModTime(),
		}
		return nil
	})
	return files, skipped, oversized, err
}

// isExcluded matches every path segment against the exclude patterns, so
// ".icloud-synk" excludes the whole subtree wherever it appears.
func (s *Scanner) isExcluded(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" {
			continue
		}
		for _, pat := range s.Exclude {
			if ok, _ := path.Match(pat, seg); ok {
				return true
			}
		}
	}
	return false
}

// IsExcluded reports whether a relative path is filtered out.
func (s *Scanner) IsExcluded(rel string) bool { return s.isExcluded(rel) }

func relSlash(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// fileSHA256 hashes a file's contents. Used only to break ties when size
// matches but mtime differs, so we do not re-upload untouched files.
func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
