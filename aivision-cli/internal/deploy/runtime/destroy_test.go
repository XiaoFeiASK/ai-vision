package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func newDestroyTestApp(t *testing.T, workdir, sourceRoot string, repos ...Repo) *App {
	t.Helper()
	bp := &Blueprint{
		Metadata: Metadata{Namespace: "aivision"},
		Config:   Config{Workdir: workdir, SourceRoot: sourceRoot},
		Components: []Component{
			{Name: "demo", Kind: "process", Run: "true"},
		},
	}
	for i, repo := range repos {
		bp.Components = append(bp.Components, Component{
			Name: "repo" + strconv.Itoa(i),
			Kind: "process",
			Run:  "true",
			Repo: &repo,
		})
	}
	return &App{
		Log:    NewLogger(false),
		BP:     bp,
		State:  NewState(workdir, "aivision"),
		Vars:   map[string]string{"SOURCE_ROOT": sourceRoot, "WORKDIR": workdir},
		Ctx:    context.Background(),
		Docker: NewDocker(NewLogger(false)),
	}
}

func TestDestroyPurgeRefusesStateRootOutsideWorkdir(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(filepath.Dir(workdir), "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	app := newDestroyTestApp(t, workdir, t.TempDir())
	app.State = NewState(workdir, "..")

	if err := app.Destroy(DestroyOpts{Purge: true}); err == nil {
		t.Fatal("destroy --purge accepted a state root outside workdir")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("directory outside workdir was removed: %v", err)
	}
	if _, err := os.Stat(workdir); err != nil {
		t.Fatalf("workdir itself was removed: %v", err)
	}
}

func TestDestroyPurgeReposKeepsPreExistingCheckout(t *testing.T) {
	sourceRoot := t.TempDir()
	existing := filepath.Join(sourceRoot, "existing")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "important.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := newDestroyTestApp(t, t.TempDir(), sourceRoot, Repo{Path: "existing"})

	if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(existing, "important.txt")); err != nil {
		t.Fatalf("pre-existing checkout was removed: %v", err)
	}
}

func TestDestroyPurgeReposRefusesReposOutsideSourceRoot(t *testing.T) {
	workdir := t.TempDir()
	base := t.TempDir()
	sourceRoot := filepath.Join(base, "src")
	victim := filepath.Join(base, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "important.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, repo := range map[string]Repo{
		"relative": {Path: "../victim"},
		"absolute": {Path: victim},
	} {
		t.Run(name, func(t *testing.T) {
			app := newDestroyTestApp(t, workdir, sourceRoot, repo)
			if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err == nil {
				t.Fatal("destroy --purge-repos accepted a repo outside sourceRoot")
			}
			if _, err := os.Stat(filepath.Join(victim, "important.txt")); err != nil {
				t.Fatalf("directory outside sourceRoot was removed: %v", err)
			}
		})
	}
}

func TestDestroyPurgeReposDeletesRecordedCloneOnly(t *testing.T) {
	sourceRoot := t.TempDir()
	cloned := filepath.Join(sourceRoot, "cloned")
	if err := os.MkdirAll(cloned, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloned, "file"), []byte("cloned"), 0o600); err != nil {
		t.Fatal(err)
	}
	app := newDestroyTestApp(t, t.TempDir(), sourceRoot, Repo{Path: "cloned"})
	if err := app.State.RecordManagedRepo(cloned); err != nil {
		t.Fatal(err)
	}

	if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cloned); !os.IsNotExist(err) {
		t.Fatalf("recorded aiv clone still exists: %v", err)
	}
}

func TestDestroyPurgeReposRejectsStaleRecordOutsideSourceRoot(t *testing.T) {
	sourceRoot := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	app := newDestroyTestApp(t, t.TempDir(), sourceRoot)
	if err := app.State.RecordManagedRepo(victim); err != nil {
		t.Fatal(err)
	}

	if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err == nil {
		t.Fatal("destroy accepted a recorded repo outside sourceRoot")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("recorded path outside sourceRoot was removed: %v", err)
	}
}
