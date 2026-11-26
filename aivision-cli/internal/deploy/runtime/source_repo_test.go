package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antgroup/ai-vision/aivision-cli/internal/blueprint"
)

func TestForceSourceFetchRequiresConfiguredOrigin(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "previous-origin")
	checkout := filepath.Join(root, "checkout")
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("-c", "init.defaultBranch=main", "init", "--quiet", origin)
	git("-C", origin, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
		"commit", "--quiet", "--allow-empty", "-m", "initial source")
	git("clone", "--quiet", origin, checkout)
	selected := filepath.Join(root, "configured-origin")
	component := Component{Name: "backend", Repo: &blueprint.Repo{URL: selected, Branch: "main", Path: checkout}}
	app := &App{Log: NewLogger(false), Ctx: context.Background(), Force: true}

	if err := app.gitCloneOrFetch(component); err == nil {
		t.Fatal("forced source fetch accepted a checkout with a different origin")
	}
	if got := git("-C", checkout, "remote", "get-url", "origin"); got != origin {
		t.Fatalf("mismatched checkout origin changed to %q", got)
	}
	app.Force = false
	if err := app.gitCloneOrFetch(component); err != nil {
		t.Fatalf("a non-forced local checkout should be reusable: %v", err)
	}
	app.Force = true
	component.Repo.URL = origin
	if err := app.gitCloneOrFetch(component); err != nil {
		t.Fatalf("forced source fetch with the configured origin failed: %v", err)
	}
}

func TestCloneRecordsManagedRepo(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	runGit := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("-c", "init.defaultBranch=main", "init", "--quiet", origin)
	runGit("-C", origin, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
		"commit", "--quiet", "--allow-empty", "-m", "initial source")

	sourceRoot := filepath.Join(root, "src")
	workdir := filepath.Join(root, "work")
	app := &App{
		Log:   NewLogger(false),
		Ctx:   context.Background(),
		State: NewState(workdir, "aivision"),
		Vars:  map[string]string{"SOURCE_ROOT": sourceRoot},
	}
	component := Component{Name: "backend", Repo: &blueprint.Repo{URL: origin, Branch: "main", Path: "backend"}}
	if err := app.gitCloneOrFetch(component); err != nil {
		t.Fatal(err)
	}
	repos, err := app.State.ManagedRepos()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(sourceRoot, "backend")
	if len(repos) != 1 || repos[0].Path != want || repos[0].ID == "" {
		t.Fatalf("managed repos = %v, want [%s]", repos, want)
	}
	if _, err := os.Stat(filepath.Join(want, ".git")); err != nil {
		t.Fatalf("clone was not created: %v", err)
	}
	id, err := repoOwnerID(want)
	if err != nil {
		t.Fatal(err)
	}
	if id != repos[0].ID {
		t.Fatalf("ownership id = %q, record = %q", id, repos[0].ID)
	}
}

func TestCloneAtSourceRootIsNotRecorded(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	runGit := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("-c", "init.defaultBranch=main", "init", "--quiet", origin)
	runGit("-C", origin, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
		"commit", "--quiet", "--allow-empty", "-m", "initial source")

	sourceRoot := filepath.Join(root, "src")
	app := &App{
		Log:   NewLogger(false),
		Ctx:   context.Background(),
		State: NewState(filepath.Join(root, "work"), "aivision"),
		Vars:  map[string]string{"SOURCE_ROOT": sourceRoot},
	}
	component := Component{Name: "backend", Repo: &blueprint.Repo{URL: origin, Branch: "main", Path: "."}}
	if err := app.gitCloneOrFetch(component); err != nil {
		t.Fatal(err)
	}
	repos, err := app.State.ManagedRepos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 0 {
		t.Fatalf("clone at sourceRoot was recorded: %v", repos)
	}
}
