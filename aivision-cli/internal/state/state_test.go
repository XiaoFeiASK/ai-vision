package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRemoveAllRejectsEscapingNamespace(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(filepath.Dir(workdir), "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "important.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, namespace := range []string{"..", "."} {
		t.Run(namespace, func(t *testing.T) {
			s := NewState(workdir, namespace)
			if err := s.RemoveAll(); err == nil {
				t.Fatalf("RemoveAll for namespace %q succeeded", namespace)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(victim, "important.txt")); err != nil {
		t.Fatalf("directory outside workdir was removed: %v", err)
	}
	if _, err := os.Stat(workdir); err != nil {
		t.Fatalf("workdir itself was removed: %v", err)
	}
}

func TestRemoveAllDeletesNamespaceRoot(t *testing.T) {
	workdir := t.TempDir()
	s := NewState(workdir, "aivision")
	if err := s.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Root); !os.IsNotExist(err) {
		t.Fatalf("state root still exists: %v", err)
	}
	if _, err := os.Stat(workdir); err != nil {
		t.Fatalf("workdir should be preserved: %v", err)
	}
}

func TestManagedReposRoundTrip(t *testing.T) {
	s := NewState(t.TempDir(), "aivision")
	repos, err := s.ManagedRepos()
	if err != nil || len(repos) != 0 {
		t.Fatalf("ManagedRepos before recording = %v, %v", repos, err)
	}
	dir := filepath.Join(t.TempDir(), "repo")
	if err := s.RecordManagedRepo(dir, "first"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordManagedRepo(dir, "second"); err != nil {
		t.Fatal(err)
	}
	repos, err = s.ManagedRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].Path != dir || repos[0].ID != "second" {
		t.Fatalf("ManagedRepos = %v, want one record for %s with id second", repos, dir)
	}
}

func TestRecordManagedRepoPreservesPendingRemoval(t *testing.T) {
	s := NewState(t.TempDir(), "aivision")
	dir := filepath.Join(t.TempDir(), "repo")
	if err := s.RecordManagedRepo(dir, "old-id"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRepoPending(dir, "old-id", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordManagedRepo(dir, "new-id"); err != nil {
		t.Fatal(err)
	}
	repos, err := s.ManagedRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("records = %v, want pending old-id and active new-id", repos)
	}
	seen := map[string]ManagedRepo{}
	for _, repo := range repos {
		seen[repo.ID] = repo
	}
	if !seen["old-id"].Pending {
		t.Fatalf("pending record was overwritten: %v", repos)
	}
	if seen["new-id"].Pending {
		t.Fatalf("replacement recorded as pending: %v", repos)
	}

	if err := s.ForgetManagedRepoID(dir, "old-id"); err != nil {
		t.Fatal(err)
	}
	repos, err = s.ManagedRepos()
	if err != nil || len(repos) != 1 || repos[0].ID != "new-id" {
		t.Fatalf("forget by id removed the wrong record: %v, %v", repos, err)
	}
}

func TestConcurrentRecordManagedRepoKeepsEveryRecord(t *testing.T) {
	s := NewState(t.TempDir(), "aivision")
	const count = 12
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir := filepath.Join(t.TempDir(), "repo")
			if err := s.RecordManagedRepo(dir, strconv.Itoa(i)); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	repos, err := s.ManagedRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != count {
		t.Fatalf("records = %d, want %d: %v", len(repos), count, repos)
	}
}

func TestRecordLockDoesNotReclaimLiveHolder(t *testing.T) {
	s := NewState(t.TempDir(), "aivision")
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.withRecordLock(func(*recordStore) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(s.lockFile(), old, old); err != nil {
		t.Fatal(err)
	}
	second := make(chan struct{})
	go func() {
		_ = s.withRecordLock(func(*recordStore) error {
			close(second)
			return nil
		})
	}()
	select {
	case <-second:
		t.Fatal("reclaimed a lock held by a live operation")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not acquire the released lock")
	}
}

func TestLongNamespaceUsesBoundedRecordAndLockNames(t *testing.T) {
	namespace := strings.Repeat("a", 240)
	s := NewState(t.TempDir(), namespace)
	repo := filepath.Join(t.TempDir(), "repo")
	if err := s.RecordManagedRepo(repo, "id"); err != nil {
		t.Fatal(err)
	}
	if name := filepath.Base(s.reposFile()); len(name) > 64 {
		t.Fatalf("record filename is not bounded: %d bytes", len(name))
	}
	if name := filepath.Base(s.lockFile()); len(name) > 64 {
		t.Fatalf("lock filename is not bounded: %d bytes", len(name))
	}
	repos, err := s.ManagedRepos()
	if err != nil || len(repos) != 1 || repos[0].Path != repo {
		t.Fatalf("records with a long namespace = %v, %v", repos, err)
	}
}

func TestNamespaceLengthsAroundLegacyRecordLimit(t *testing.T) {
	for _, length := range []int{240, 250, 251, 254} {
		t.Run(strconv.Itoa(length), func(t *testing.T) {
			s := NewState(t.TempDir(), strings.Repeat("a", length))
			repo := filepath.Join(t.TempDir(), "repo")
			if err := s.RecordManagedRepo(repo, "id"); err != nil {
				t.Fatal(err)
			}
			repos, err := s.ManagedRepos()
			if err != nil || len(repos) != 1 || repos[0].Path != repo {
				t.Fatalf("records with a %d-character namespace = %v, %v", length, repos, err)
			}
		})
	}

	s := NewState(t.TempDir(), strings.Repeat("a", 300))
	if err := s.RecordManagedRepo(filepath.Join(t.TempDir(), "repo"), "id"); err == nil {
		t.Fatal("RecordManagedRepo accepted a namespace that cannot be a directory name")
	}
}

func TestRecordStoreStaysWritableAfterConcurrentCleanup(t *testing.T) {
	workdir := t.TempDir()
	s := NewState(workdir, "aivision")
	store, err := s.openRecordStore(true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := s.RecordManagedRepo(repo, "id"); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetManagedRepo(repo); err != nil {
		t.Fatal(err)
	}
	if err := store.writeFileAtomic(s.reposFile(), []byte("[]"), 0o644); err != nil {
		t.Fatalf("record store opened before cleanup became unusable: %v", err)
	}
	repos, err := s.ManagedRepos()
	if err != nil || len(repos) != 0 {
		t.Fatalf("records after concurrent write = %v, %v", repos, err)
	}
}

func TestRemoveAllDoesNotDependOnRecordStorage(t *testing.T) {
	workdir := t.TempDir()
	s := NewState(workdir, "aivision")
	if err := s.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(workdir, ".aivision-repos")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAll(); err != nil {
		t.Fatalf("state-only purge depended on record storage: %v", err)
	}
	if _, err := os.Stat(s.Root); !os.IsNotExist(err) {
		t.Fatalf("state root still exists: %v", err)
	}
	if _, err := os.Stat(blocker); err != nil {
		t.Fatalf("unrelated record path was modified: %v", err)
	}
}

func TestManagedReposSurviveStatePurge(t *testing.T) {
	workdir := t.TempDir()
	s := NewState(workdir, "aivision")
	if err := s.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "repo")
	if err := s.RecordManagedRepo(dir, "id"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	repos, err := s.ManagedRepos()
	if err != nil || len(repos) != 1 || repos[0].Path != dir || repos[0].ID != "id" {
		t.Fatalf("records after state purge = %v, %v", repos, err)
	}
	if err := s.ForgetManagedRepo(dir); err != nil {
		t.Fatal(err)
	}
	repos, err = s.ManagedRepos()
	if err != nil || len(repos) != 0 {
		t.Fatalf("records after forget = %v, %v", repos, err)
	}
	entries, err := os.ReadDir(s.recordDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("records remain after forget: %v", entries)
	}
}

func TestManagedReposReadsLegacyRecord(t *testing.T) {
	workdir := t.TempDir()
	s := NewState(workdir, "aivision")
	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "repo")
	legacy, err := json.Marshal([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.legacyReposFile(), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	repos, err := s.ManagedRepos()
	if err != nil || len(repos) != 1 || repos[0].Path != dir || repos[0].ID != "" {
		t.Fatalf("legacy records = %v, %v", repos, err)
	}
	if err := s.RecordManagedRepo(filepath.Join(t.TempDir(), "other"), "id"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.legacyReposFile()); !os.IsNotExist(err) {
		t.Fatalf("legacy record file was not migrated: %v", err)
	}
}

func TestManagedReposReadsNamedRecord(t *testing.T) {
	workdir := t.TempDir()
	s := NewState(workdir, "aivision")
	if err := os.MkdirAll(s.recordDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "repo")
	named, err := json.Marshal([]ManagedRepo{{Path: dir, ID: "old-id"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.namedReposFile(), named, 0o600); err != nil {
		t.Fatal(err)
	}
	repos, err := s.ManagedRepos()
	if err != nil || len(repos) != 1 || repos[0].ID != "old-id" {
		t.Fatalf("named records = %v, %v", repos, err)
	}

	other := filepath.Join(t.TempDir(), "other")
	if err := s.RecordManagedRepo(other, "new-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.namedReposFile()); !os.IsNotExist(err) {
		t.Fatalf("named record file was not migrated: %v", err)
	}
	repos, err = s.ManagedRepos()
	if err != nil || len(repos) != 2 {
		t.Fatalf("records after migration = %v, %v", repos, err)
	}
}

func TestRecordTargetsStayInsideWorkdir(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewState(workdir, "../../escape")
	recordPath := filepath.Join(filepath.Dir(workdir), "escape.json")
	if err := os.WriteFile(recordPath, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ManagedRepos(); err == nil {
		t.Fatal("ManagedRepos accepted a record path outside workdir")
	}
	if err := s.RecordManagedRepo(filepath.Join(t.TempDir(), "repo"), "id"); err == nil {
		t.Fatal("RecordManagedRepo accepted a record path outside workdir")
	}
	data, err := os.ReadFile(recordPath)
	if err != nil || string(data) != "pending" {
		t.Fatalf("pending record changed on refusal: %q, %v", data, err)
	}
}

func TestRecordTargetsRejectRootOutsideWorkdir(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := NewState(workdir, "..")
	legacy := filepath.Join(filepath.Dir(workdir), "state", "repos.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.VerifyRecordTargets(); err == nil {
		t.Fatal("VerifyRecordTargets accepted a state root outside workdir")
	}
	if err := s.RecordManagedRepo(filepath.Join(t.TempDir(), "repo"), "id"); err == nil {
		t.Fatal("RecordManagedRepo accepted a state root outside workdir")
	}
	data, err := os.ReadFile(legacy)
	if err != nil || string(data) != "pending" {
		t.Fatalf("legacy record outside workdir changed: %q, %v", data, err)
	}
}

func TestRecordTargetsRejectSymlinkedStorageWithoutWriting(t *testing.T) {
	workdir := t.TempDir()
	outside := t.TempDir()
	s := NewState(workdir, "aivision")
	if err := os.Symlink(outside, filepath.Join(workdir, ".aivision-repos")); err != nil {
		t.Fatal(err)
	}

	if _, err := s.ManagedRepos(); err == nil {
		t.Fatal("ManagedRepos followed a record directory outside workdir")
	}
	if err := s.RecordManagedRepo(filepath.Join(t.TempDir(), "repo"), "id"); err == nil {
		t.Fatal("RecordManagedRepo wrote through a symlinked record directory")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("out-of-scope directory was modified: %v", entries)
	}
}

func TestRecordStoreStaysBoundToOpenedRecordDirectory(t *testing.T) {
	workdir := t.TempDir()
	s := NewState(workdir, "aivision")
	store, err := s.openRecordStore(true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	moved := s.recordDir() + "-moved"
	if err := os.Rename(s.recordDir(), moved); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, s.recordDir()); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := store.writeFileAtomic(s.reposFile(), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(moved, filepath.Base(s.reposFile()))); err != nil {
		t.Fatalf("record was not written through the opened directory: %v", err)
	}
	if entries, err := os.ReadDir(elsewhere); err != nil || len(entries) != 0 {
		t.Fatalf("record escaped through the replaced path: %v, %v", entries, err)
	}
}

func TestRecordStorageUnavailableFailsClosed(t *testing.T) {
	workdir := t.TempDir()
	s := NewState(workdir, "aivision")
	if err := os.WriteFile(s.recordDir(), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordManagedRepo(filepath.Join(t.TempDir(), "repo"), "id"); err == nil {
		t.Fatal("RecordManagedRepo wrote through a non-directory record path")
	}
	data, err := os.ReadFile(s.recordDir())
	if err != nil || string(data) != "keep" {
		t.Fatalf("unrelated record path changed: %q, %v", data, err)
	}
}
