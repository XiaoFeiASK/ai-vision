package state

import (
	"os"
	"path/filepath"
	"testing"
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
	if err := s.RecordManagedRepo(dir); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordManagedRepo(dir); err != nil {
		t.Fatal(err)
	}
	repos, err = s.ManagedRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0] != dir {
		t.Fatalf("ManagedRepos = %v, want [%s]", repos, dir)
	}
}
