// Package state persists everything the CLI records about a deployment
// (pidfiles, once-markers, deploy metadata) so status, logs and destroy can
// operate even when the blueprint is not at hand.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antgroup/ai-vision/aivision-cli/internal/blueprint"
	"github.com/antgroup/ai-vision/aivision-cli/internal/pathguard"
)

// State tracks everything aiv persists about a deployment so that status,
// logs and destroy can operate even when the blueprint is not at hand.
type State struct {
	Namespace   string
	Workdir     string // resolved workdir; Root must stay strictly inside it
	Root        string // <workdir>/<namespace>
	StateDir    string // Root/state
	LogDir      string // Root/logs
	ArtifactDir string // Root/artifacts
}

// DeployMeta is written to state/meta.json on every successful cast.
type DeployMeta struct {
	DatastoreBackend string            `json:"datastoreBackend,omitempty"`
	Namespace        string            `json:"namespace"`
	Blueprint        string            `json:"blueprint"`
	SourceRoot       string            `json:"sourceRoot"`
	DeployedAt       string            `json:"deployedAt"`
	Components       map[string]string `json:"components"` // name -> kind
	Containers       map[string]string `json:"containers"` // component -> container name
	Discovered       map[string]string `json:"discovered"` // var -> value (e.g. ORG_ID)
}

func NewState(workdir, namespace string) *State {
	workdir = blueprint.CleanPath(workdir, nil)
	root := filepath.Join(workdir, namespace)
	return &State{
		Namespace:   namespace,
		Workdir:     workdir,
		Root:        root,
		StateDir:    filepath.Join(root, "state"),
		LogDir:      filepath.Join(root, "logs"),
		ArtifactDir: filepath.Join(root, "artifacts"),
	}
}

func (s *State) EnsureDirs() error {
	for _, d := range []string{s.StateDir, s.LogDir, s.ArtifactDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("create dir %s: %w", d, err)
		}
	}
	return nil
}

// PidFile returns the pidfile path for a process component.
func (s *State) PidFile(comp string) string {
	return filepath.Join(s.StateDir, comp+".pid")
}

// LogFile returns the log path for a component.
func (s *State) LogFile(comp string) string {
	return filepath.Join(s.LogDir, comp+".log")
}

func (s *State) WritePid(comp string, pid int) error {
	return os.WriteFile(s.PidFile(comp), []byte(strconv.Itoa(pid)), 0o644)
}

func (s *State) ReadPid(comp string) int {
	b, err := os.ReadFile(s.PidFile(comp))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}

func (s *State) RemovePid(comp string) {
	_ = os.Remove(s.PidFile(comp))
}

// PidAlive reports whether a pid exists and is running. It probes with
// signal 0 (syscall.Kill) which never actually delivers a signal.
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// markerFile returns the once-marker path for a component step.
func (s *State) markerFile(comp, step string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == ' ' {
			return '_'
		}
		return r
	}, step)
	if len(safe) > 48 {
		safe = safe[:48]
	}
	return filepath.Join(s.StateDir, fmt.Sprintf("%s.%s.done", comp, safe))
}

func (s *State) MarkerDone(comp, step string) bool {
	_, err := os.Stat(s.markerFile(comp, step))
	return err == nil
}

func (s *State) MarkDone(comp, step string) error {
	return os.WriteFile(s.markerFile(comp, step), []byte(time.Now().Format(time.RFC3339)), 0o644)
}

// ClearMarkers removes all once-markers (used by destroy --purge and --force-bootstrap).
func (s *State) ClearMarkers() {
	entries, _ := os.ReadDir(s.StateDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".done") {
			_ = os.Remove(filepath.Join(s.StateDir, e.Name()))
		}
	}
}

func (s *State) metaFile() string { return filepath.Join(s.StateDir, "meta.json") }

func (s *State) SaveMeta(m DeployMeta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.metaFile(), b, 0o644)
}

func (s *State) LoadMeta() (*DeployMeta, error) {
	b, err := os.ReadFile(s.metaFile())
	if err != nil {
		return nil, err
	}
	var m DeployMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// varsFile holds the user-supplied variable overrides that produced the last
// successful cast, so status/destroy/redeploy reuse them without the caller
// re-passing every --var. It may contain secrets, hence 0600.
func (s *State) varsFile() string { return filepath.Join(s.StateDir, "vars.json") }

// SaveVars persists the override set (not the blueprint defaults).
func (s *State) SaveVars(vars map[string]string) error {
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.varsFile(), b, 0o600)
}

// LoadVars returns the persisted overrides, or an empty map when none exist.
func (s *State) LoadVars() (map[string]string, error) {
	b, err := os.ReadFile(s.varsFile())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := map[string]string{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.varsFile(), err)
	}
	return out, nil
}

// VerifyPurgeTarget checks that the state root is a real child of the
// configured workdir, so a namespace such as "..", "." or an absolute path
// can never widen the delete.
func (s *State) VerifyPurgeTarget() error {
	if s.Workdir == "" {
		return fmt.Errorf("refusing to purge %s: state root has no workdir", s.Root)
	}
	if err := pathguard.EnsureStrictlyWithin(s.Workdir, s.Root); err != nil {
		return fmt.Errorf("refusing to purge state dir: %w", err)
	}
	return nil
}

// RemoveAll deletes the entire namespace state root (destroy --purge).
func (s *State) RemoveAll() error {
	if err := s.VerifyPurgeTarget(); err != nil {
		return err
	}
	return os.RemoveAll(s.Root)
}

// ManagedRepo records a checkout aiv cloned. ID matches the ownership marker
// in the checkout's .git directory and is verified again before deletion.
type ManagedRepo struct {
	Path string `json:"path"`
	ID   string `json:"id,omitempty"`
	// Pending marks a removal that was started but not completed. The record
	// stays until the tombstone is gone, so a retry can finish the cleanup.
	Pending bool `json:"pending,omitempty"`
}

// reposFile lives outside the namespace root so a state-only --purge keeps
// the ownership records needed by a later --purge-repos.
func (s *State) reposFile() string {
	return filepath.Join(s.Workdir, ".aivision-repos", s.Namespace+".json")
}

// legacyReposFile is the record location used before records moved out of the
// purgeable state directory.
func (s *State) legacyReposFile() string { return filepath.Join(s.StateDir, "repos.json") }

// VerifyRecordTargets keeps both record locations inside their intended
// storage: current records inside workdir, legacy records inside the state
// root. Callers check this before writing, cleaning up or tearing down.
func (s *State) VerifyRecordTargets() error {
	if s.Workdir == "" {
		return fmt.Errorf("ownership records have no workdir")
	}
	if err := pathguard.EnsureStrictlyWithin(s.Workdir, s.Root); err != nil {
		return fmt.Errorf("refusing ownership records: %w", err)
	}
	if err := pathguard.EnsureStrictlyWithin(s.Workdir, s.reposFile()); err != nil {
		return fmt.Errorf("refusing ownership records: %w", err)
	}
	if err := pathguard.EnsureStrictlyWithin(s.Root, s.legacyReposFile()); err != nil {
		return fmt.Errorf("refusing legacy ownership records: %w", err)
	}
	return nil
}

func readManagedRepos(path string) ([]ManagedRepo, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var records []ManagedRepo
	if err := json.Unmarshal(b, &records); err == nil {
		return records, nil
	}
	var legacy []string
	if err := json.Unmarshal(b, &legacy); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	records = make([]ManagedRepo, 0, len(legacy))
	for _, dir := range legacy {
		records = append(records, ManagedRepo{Path: dir})
	}
	return records, nil
}

// ManagedRepos returns the recorded aiv-cloned repositories, merging the
// current location with the legacy in-state file so a pre-upgrade record is
// not lost while it still exists.
func (s *State) ManagedRepos() ([]ManagedRepo, error) {
	return s.managedReposUnlocked()
}

func (s *State) managedReposUnlocked() ([]ManagedRepo, error) {
	var out []ManagedRepo
	seen := map[string]bool{}
	seenPath := map[string]bool{}
	for _, path := range []string{s.reposFile(), s.legacyReposFile()} {
		if err := s.VerifyRecordTargets(); err != nil {
			return nil, err
		}
		records, err := readManagedRepos(path)
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			dir, err := filepath.Abs(record.Path)
			if err != nil {
				return nil, err
			}
			record.Path = filepath.Clean(dir)
			key := record.Path + "\x00" + record.ID
			if record.Pending {
				key += "\x00pending"
			}
			if seen[key] {
				continue
			}
			if record.ID == "" && seenPath[record.Path] {
				continue
			}
			seen[key] = true
			seenPath[record.Path] = true
			out = append(out, record)
		}
	}
	return out, nil
}

func (s *State) writeManagedReposLocked(repos []ManagedRepo) error {
	if err := s.VerifyRecordTargets(); err != nil {
		return err
	}
	path := s.reposFile()
	if len(repos) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := s.VerifyRecordTargets(); err != nil {
			return err
		}
		_ = os.Remove(s.legacyReposFile())
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(repos, "", "  ")
	if err != nil {
		return err
	}
	if err := s.VerifyRecordTargets(); err != nil {
		return err
	}
	if err := writeFileAtomic(path, b, 0o644); err != nil {
		return err
	}
	if err := s.VerifyRecordTargets(); err != nil {
		return err
	}
	_ = os.Remove(s.legacyReposFile())
	return nil
}

// withRecordLock serialises ownership-record updates across CLI processes.
// The lock and the record file both live under workdir, outside the state
// directory removed by --purge.
func (s *State) withRecordLock(fn func() error) error {
	if err := s.VerifyRecordTargets(); err != nil {
		return err
	}
	dir := filepath.Dir(s.reposFile())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := s.VerifyRecordTargets(); err != nil {
		return err
	}
	lock := filepath.Join(dir, s.Namespace+".lock")
	if err := acquireRecordLock(lock); err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(lock)
		_ = os.Remove(dir)
	}()
	if err := s.VerifyRecordTargets(); err != nil {
		return err
	}
	return fn()
}

func acquireRecordLock(path string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return nil
		}
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
				return mkErr
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out waiting for ownership record lock %s", path)
			}
			continue
		}
		if !os.IsExist(err) {
			return err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 2*time.Minute {
			if os.RemoveAll(path) == nil {
				continue
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for ownership record lock %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *State) updateManagedRepos(mutate func([]ManagedRepo) []ManagedRepo) error {
	return s.withRecordLock(func() error {
		repos, err := s.managedReposUnlocked()
		if err != nil {
			return err
		}
		return s.writeManagedReposLocked(mutate(repos))
	})
}

// RecordManagedRepo marks dir as cloned by aiv so a later --purge-repos can
// delete it. Paths outside sourceRoot are not recorded; destroy keeps them
// even if a stale record exists.
func (s *State) RecordManagedRepo(dir, id string) error {
	if id == "" {
		return fmt.Errorf("record cloned repo %s: empty ownership id", dir)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	return s.updateManagedRepos(func(repos []ManagedRepo) []ManagedRepo {
		out := make([]ManagedRepo, 0, len(repos)+1)
		recorded := false
		for _, repo := range repos {
			if repo.Path != abs {
				out = append(out, repo)
				continue
			}
			if repo.ID == id {
				// Keep Pending so an interrupted removal is not cancelled by
				// a checkout that reappears at the same path.
				out = append(out, repo)
				recorded = true
				continue
			}
			if repo.Pending {
				out = append(out, repo)
			}
		}
		if !recorded {
			out = append(out, ManagedRepo{Path: abs, ID: id})
		}
		return out
	})
}

// ForgetManagedRepo removes dir from the ownership records after its checkout
// has been deleted.
func (s *State) ForgetManagedRepo(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	return s.updateManagedRepos(func(repos []ManagedRepo) []ManagedRepo {
		kept := make([]ManagedRepo, 0, len(repos))
		for _, repo := range repos {
			if repo.Path != abs {
				kept = append(kept, repo)
			}
		}
		return kept
	})
}

// ForgetManagedRepoID removes only the record for one ownership id, keeping a
// newer checkout recorded at the same path.
func (s *State) ForgetManagedRepoID(dir, id string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	return s.updateManagedRepos(func(repos []ManagedRepo) []ManagedRepo {
		kept := make([]ManagedRepo, 0, len(repos))
		for _, repo := range repos {
			if repo.Path != abs || repo.ID != id {
				kept = append(kept, repo)
			}
		}
		return kept
	})
}

// SetRepoPending records that removal of dir has started. It requires a
// matching record so an unrecorded checkout is never treated as pending.
func (s *State) SetRepoPending(dir, id string, pending bool) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	found := false
	err = s.updateManagedRepos(func(repos []ManagedRepo) []ManagedRepo {
		for i := range repos {
			if repos[i].Path == abs && repos[i].ID == id {
				repos[i].Pending = pending
				found = true
			}
		}
		return repos
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("ownership record for %s is missing", dir)
	}
	return nil
}
