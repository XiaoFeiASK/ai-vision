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

// makeClone creates a checkout with the ownership marker aiv writes after a
// successful clone and records it with the given id.
func makeClone(t *testing.T, app *App, dir, recordedID, markerID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("cloned"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeRepoOwner(dir, markerID); err != nil {
		t.Fatal(err)
	}
	if recordedID != "" {
		if err := app.State.RecordManagedRepo(dir, recordedID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDestroyPurgeValidatesStateBeforeTeardown(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(filepath.Dir(workdir), "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	teardownRan := filepath.Join(t.TempDir(), "teardown-ran")
	app := newDestroyTestApp(t, workdir, t.TempDir())
	app.BP.Components[0].Teardown = []Step{{Shell: "touch " + teardownRan}}
	app.State = NewState(workdir, "..")

	if err := app.Destroy(DestroyOpts{Purge: true}); err == nil {
		t.Fatal("destroy --purge accepted a state root outside workdir")
	}
	if _, err := os.Stat(teardownRan); !os.IsNotExist(err) {
		t.Fatalf("teardown ran before the state purge target was validated: %v", err)
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

func TestDestroyPurgeReposRefusesBlueprintReposOutsideSourceRoot(t *testing.T) {
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

func TestDestroyPurgeReposDeletesVerifiedClone(t *testing.T) {
	sourceRoot := t.TempDir()
	cloned := filepath.Join(sourceRoot, "cloned")
	app := newDestroyTestApp(t, t.TempDir(), sourceRoot, Repo{Path: "cloned"})
	makeClone(t, app, cloned, "owner-id", "owner-id")

	if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cloned); !os.IsNotExist(err) {
		t.Fatalf("verified aiv clone still exists: %v", err)
	}
	repos, err := app.State.ManagedRepos()
	if err != nil || len(repos) != 0 {
		t.Fatalf("records after purge = %v, %v", repos, err)
	}
}

func TestDestroyPurgeReposKeepsCheckoutWithMismatchedOwner(t *testing.T) {
	sourceRoot := t.TempDir()
	replaced := filepath.Join(sourceRoot, "replaced")
	app := newDestroyTestApp(t, t.TempDir(), sourceRoot, Repo{Path: "replaced"})
	makeClone(t, app, replaced, "expected-id", "different-id")

	if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(replaced, "file")); err != nil {
		t.Fatalf("checkout with mismatched ownership was removed: %v", err)
	}
}

func TestDestroyPurgeReposSkipsUnusableRecordsAndCleansOthers(t *testing.T) {
	workdir := t.TempDir()
	sourceRoot := t.TempDir()
	app := newDestroyTestApp(t, workdir, sourceRoot)

	outside := filepath.Join(t.TempDir(), "outside")
	makeClone(t, app, outside, "outside-id", "outside-id")
	makeClone(t, app, sourceRoot, "root-id", "root-id")
	valid := filepath.Join(sourceRoot, "valid")
	makeClone(t, app, valid, "valid-id", "valid-id")

	if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("record outside sourceRoot was removed: %v", err)
	}
	if _, err := os.Stat(sourceRoot); err != nil {
		t.Fatalf("sourceRoot itself was removed: %v", err)
	}
	if _, err := os.Stat(valid); !os.IsNotExist(err) {
		t.Fatalf("eligible clone was not removed: %v", err)
	}
}

func TestDestroyPurgeOnlyKeepsRepoRecords(t *testing.T) {
	sourceRoot := t.TempDir()
	cloned := filepath.Join(sourceRoot, "cloned")
	app := newDestroyTestApp(t, t.TempDir(), sourceRoot, Repo{Path: "cloned"})
	makeClone(t, app, cloned, "owner-id", "owner-id")

	if err := app.Destroy(DestroyOpts{Purge: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cloned, "file")); err != nil {
		t.Fatalf("state-only purge removed the clone: %v", err)
	}
	repos, err := app.State.ManagedRepos()
	if err != nil || len(repos) != 1 || repos[0].Path != cloned {
		t.Fatalf("records after state-only purge = %v, %v", repos, err)
	}

	if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cloned); !os.IsNotExist(err) {
		t.Fatalf("clone was not removed by the retry: %v", err)
	}
}

func TestDestroyPurgeReposKeepsRecordWhenRemovalFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based removal failure does not apply to root")
	}
	sourceRoot := t.TempDir()
	cloned := filepath.Join(sourceRoot, "cloned")
	app := newDestroyTestApp(t, t.TempDir(), sourceRoot, Repo{Path: "cloned"})
	makeClone(t, app, cloned, "owner-id", "owner-id")

	if err := os.Chmod(sourceRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sourceRoot, 0o755) })
	if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err == nil {
		t.Fatal("destroy reported success after the repo removal failed")
	}
	repos, err := app.State.ManagedRepos()
	if err != nil || len(repos) != 1 || repos[0].Path != cloned {
		t.Fatalf("failed cleanup did not retain its record: %v, %v", repos, err)
	}

	if err := os.Chmod(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err != nil {
		t.Fatalf("retry after restoring permissions failed: %v", err)
	}
	if _, err := os.Stat(cloned); !os.IsNotExist(err) {
		t.Fatalf("checkout survived the retry: %v", err)
	}
	repos, err = app.State.ManagedRepos()
	if err != nil || len(repos) != 0 {
		t.Fatalf("record survived the retry: %v, %v", repos, err)
	}
}

func TestDestroyPurgeReposFinishesInterruptedRemoval(t *testing.T) {
	sourceRoot := t.TempDir()
	cloned := filepath.Join(sourceRoot, "cloned")
	app := newDestroyTestApp(t, t.TempDir(), sourceRoot, Repo{Path: "cloned"})
	makeClone(t, app, cloned, "owner-id", "owner-id")

	tombstone := app.purgeTombstone(ManagedRepo{Path: cloned, ID: "owner-id"})
	if err := os.Rename(cloned, tombstone); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tombstone, "partially-removed"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	preview, err := app.PurgeRepoPreview()
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 1 || preview[0] != cloned {
		t.Fatalf("preview = %v, want pending removal %s", preview, cloned)
	}

	if err := app.Destroy(DestroyOpts{Purge: true, PurgeRepos: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tombstone); !os.IsNotExist(err) {
		t.Fatalf("pending removal survived: %v", err)
	}
	repos, err := app.State.ManagedRepos()
	if err != nil || len(repos) != 0 {
		t.Fatalf("record survived: %v, %v", repos, err)
	}
}

func TestPurgeRepoPreviewMatchesRemovalCandidates(t *testing.T) {
	sourceRoot := t.TempDir()
	app := newDestroyTestApp(t, t.TempDir(), sourceRoot)
	verified := filepath.Join(sourceRoot, "verified")
	replaced := filepath.Join(sourceRoot, "replaced")
	makeClone(t, app, verified, "verified-id", "verified-id")
	makeClone(t, app, replaced, "expected-id", "different-id")

	preview, err := app.PurgeRepoPreview()
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 1 || preview[0] != verified {
		t.Fatalf("preview = %v, want [%s]", preview, verified)
	}
}

func TestDestroyPurgeReposRefusesRecordPathOutsideWorkdir(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	app := newDestroyTestApp(t, workdir, t.TempDir())
	app.State = NewState(workdir, "../../escape")
	teardownRan := filepath.Join(t.TempDir(), "teardown-ran")
	app.BP.Components[0].Teardown = []Step{{Shell: "touch " + teardownRan}}

	recordPath := filepath.Join(filepath.Dir(workdir), "escape.json")
	if err := os.MkdirAll(filepath.Dir(recordPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := app.Destroy(DestroyOpts{PurgeRepos: true}); err == nil {
		t.Fatal("destroy accepted ownership records outside workdir")
	}
	if _, err := os.Stat(teardownRan); !os.IsNotExist(err) {
		t.Fatalf("teardown ran before record targets were validated: %v", err)
	}
	data, err := os.ReadFile(recordPath)
	if err != nil || string(data) != "pending" {
		t.Fatalf("pending record changed on refusal: %q, %v", data, err)
	}
}
