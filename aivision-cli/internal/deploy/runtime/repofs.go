package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/antgroup/ai-vision/aivision-cli/internal/pathguard"
)

var errSourceRootMissing = errors.New("source root does not exist")

// repoFS binds every repository inspection and removal operation to the
// source root that was validated, so a path component swapped for a symlink
// after the check cannot redirect the operation outside it.
type repoFS struct {
	root *os.Root
	base string
}

func openRepoFS(base string) (*repoFS, error) {
	if base == "" {
		return nil, fmt.Errorf("source root is not configured")
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errSourceRootMissing
		}
		return nil, fmt.Errorf("open source root %s: %w", base, err)
	}
	return &repoFS{root: root, base: base}, nil
}

func (f *repoFS) Close() error { return f.root.Close() }

func (f *repoFS) rel(path string) (string, error) {
	if err := pathguard.EnsureStrictlyWithin(f.base, path); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(f.base, path)
	if err != nil {
		return "", err
	}
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside %s", path, f.base)
	}
	return rel, nil
}

func (f *repoFS) lstat(path string) (os.FileInfo, error) {
	rel, err := f.rel(path)
	if err != nil {
		return nil, err
	}
	return f.root.Lstat(rel)
}

func (f *repoFS) readFile(path string) ([]byte, error) {
	rel, err := f.rel(path)
	if err != nil {
		return nil, err
	}
	return f.root.ReadFile(rel)
}

func (f *repoFS) rename(oldPath, newPath string) error {
	oldRel, err := f.rel(oldPath)
	if err != nil {
		return err
	}
	newRel, err := f.rel(newPath)
	if err != nil {
		return err
	}
	return f.root.Rename(oldRel, newRel)
}

func (f *repoFS) remove(path string) error {
	rel, err := f.rel(path)
	if err != nil {
		return err
	}
	if err := f.root.Remove(rel); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (f *repoFS) removeAll(path string) error {
	rel, err := f.rel(path)
	if err != nil {
		return err
	}
	return f.root.RemoveAll(rel)
}

func (f *repoFS) inspectRepoOwner(dir, want string) (repoOwnerState, error) {
	b, err := f.readFile(filepath.Join(dir, repoOwnerMarker))
	if err != nil {
		if os.IsNotExist(err) {
			return repoOwnerMissing, nil
		}
		return repoOwnerMissing, err
	}
	var owner repoOwner
	if err := json.Unmarshal(b, &owner); err != nil {
		return repoOwnerMissing, fmt.Errorf("parse %s: %w", filepath.Join(dir, repoOwnerMarker), err)
	}
	if owner.ID == "" {
		return repoOwnerMissing, fmt.Errorf("%s has no ownership id", filepath.Join(dir, repoOwnerMarker))
	}
	if owner.ID != want {
		return repoOwnerDifferent, nil
	}
	return repoOwnerMatches, nil
}

func (f *repoFS) writePurgeMarker(tombstone string, repo ManagedRepo) error {
	marker := purgeMarkerRecord{Path: repo.Path, ID: repo.ID}
	if dev, ino, ok := f.identity(repo.Path); ok {
		marker.HasIdentity = true
		marker.Dev = dev
		marker.Ino = ino
	}
	b, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	path := purgeMarkerPath(tombstone)
	rel, err := f.rel(path)
	if err != nil {
		return err
	}
	dirRel := filepath.Dir(rel)
	base := filepath.Base(rel)
	var file *os.File
	var tmpRel string
	for i := 0; i < 10; i++ {
		tmpRel = filepath.Join(dirRel, fmt.Sprintf(".%s.tmp-%d-%d", base, os.Getpid(), time.Now().UnixNano()))
		file, err = f.root.OpenFile(tmpRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return err
		}
	}
	if file == nil {
		return fmt.Errorf("create temporary marker for %s", path)
	}
	defer func() {
		_ = file.Close()
		_ = f.root.Remove(tmpRel)
	}()
	if _, err := file.Write(b); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := f.root.Rename(tmpRel, rel); err != nil {
		return err
	}
	if d, err := f.root.Open(dirRel); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (f *repoFS) readPurgeMarker(tombstone string) (purgeMarkerRecord, error) {
	b, err := f.readFile(purgeMarkerPath(tombstone))
	if err != nil {
		return purgeMarkerRecord{}, err
	}
	var marker purgeMarkerRecord
	if err := json.Unmarshal(b, &marker); err != nil {
		return purgeMarkerRecord{}, err
	}
	return marker, nil
}

func (f *repoFS) purgeMarkerMatches(tombstone string, repo ManagedRepo) bool {
	marker, err := f.readPurgeMarker(tombstone)
	return err == nil && marker.Path == repo.Path && marker.ID == repo.ID
}

// purgeMarkerOwnsTombstone verifies that the directory at the tombstone path
// is the object that was renamed, not merely that a removal was once planned.
func (f *repoFS) purgeMarkerOwnsTombstone(tombstone string, repo ManagedRepo) bool {
	marker, err := f.readPurgeMarker(tombstone)
	if err != nil || marker.Path != repo.Path || marker.ID != repo.ID {
		return false
	}
	if marker.HasIdentity {
		dev, ino, ok := f.identity(tombstone)
		return ok && dev == marker.Dev && ino == marker.Ino
	}
	// Without a persisted identity, only trust the tombstone when the checkout
	// is no longer at its original path, so the rename must have happened.
	if _, err := f.lstat(repo.Path); err == nil || !os.IsNotExist(err) {
		return false
	}
	return true
}
