package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDestroyPurgeReposHonorsDeclaredNestedCheckouts(t *testing.T) {
	for _, mode := range []string{"user worktree", "both managed", "inner not confirmed", "appeared after preview", "shared monorepo"} {
		t.Run(mode, func(t *testing.T) {
			src := t.TempDir()
			outer := filepath.Join(src, "outer")
			inner := filepath.Join(outer, "inner")
			nested := Repo{Path: "outer/inner"}
			if mode == "shared monorepo" {
				nested = Repo{Path: "outer", Subdir: "inner"}
			}
			app := newDestroyTestApp(t, t.TempDir(), src, Repo{Path: "outer"}, nested)
			makeClone(t, app, outer, "outer-id", "outer-id")
			addWorktree := func() {
				t.Helper()
				if err := os.MkdirAll(inner, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(inner, ".git"), []byte("gitdir: elsewhere\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "user worktree":
				addWorktree()
			case "both managed", "inner not confirmed":
				makeClone(t, app, inner, "inner-id", "inner-id")
			case "shared monorepo":
				if err := os.Mkdir(inner, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := app.PurgeRepoPlan()
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "user worktree":
				if len(plan) != 0 {
					t.Fatalf("preview included parent of user checkout: %v", plan)
				}
			case "both managed", "inner not confirmed":
				if len(plan) != 2 {
					t.Fatalf("expected both managed checkouts in preview: %v", plan)
				}
				if mode == "inner not confirmed" {
					plan = []ManagedRepo{{Path: outer, ID: "outer-id"}}
				}
			default:
				if len(plan) != 1 || plan[0].Path != outer {
					t.Fatalf("expected outer checkout in preview: %v", plan)
				}
			}
			if mode == "appeared after preview" {
				addWorktree()
			}
			if err := app.Destroy(DestroyOpts{PurgeRepos: true, ConfirmedRepos: plan}); err != nil {
				t.Fatal(err)
			}
			if mode == "both managed" || mode == "shared monorepo" {
				if _, err := os.Stat(outer); !os.IsNotExist(err) {
					t.Fatalf("outer clone survived: %v", err)
				}
				if records, err := app.State.ManagedRepos(); err != nil || len(records) != 0 {
					t.Fatalf("checkout records survived: %v, %v", records, err)
				}
			} else if _, err := os.Stat(filepath.Join(inner, ".git")); err != nil {
				t.Fatalf("nested checkout was removed: %v", err)
			}
		})
	}
}

func TestDestroyPurgeReposResumesNestedClonesIndependently(t *testing.T) {
	for _, pending := range []string{"inner", "outer"} {
		t.Run(pending, func(t *testing.T) {
			src := t.TempDir()
			outer := filepath.Join(src, "outer")
			inner := filepath.Join(outer, "inner")
			app := newDestroyTestApp(t, t.TempDir(), src, Repo{Path: "outer"}, Repo{Path: "outer/inner"})
			makeClone(t, app, outer, "outer", "outer")
			record := ManagedRepo{Path: outer, ID: "outer"}
			if pending == "inner" {
				makeClone(t, app, inner, "inner", "inner")
				record = ManagedRepo{Path: inner, ID: "inner"}
			} else if err := os.MkdirAll(filepath.Join(inner, ".git"), 0o755); err != nil {
				t.Fatal(err)
			}
			tomb := app.purgeTombstone(record)
			if err := app.State.SetRepoPending(record.Path, record.ID, true); err != nil {
				t.Fatal(err)
			}
			writeTestPurgeMarker(t, src, tomb, record)
			if err := os.Rename(record.Path, tomb); err != nil {
				t.Fatal(err)
			}
			plan, err := app.PurgeRepoPlan()
			if err != nil {
				t.Fatal(err)
			}
			if pending == "inner" && (len(plan) != 1 || plan[0].Path != inner) {
				t.Fatalf("expected only the interrupted inner removal: %v", plan)
			}
			if pending == "outer" && len(plan) != 0 {
				t.Fatalf("preview included a moved tree containing a user checkout: %v", plan)
			}
			if err := app.Destroy(DestroyOpts{PurgeRepos: true, ConfirmedRepos: plan}); err != nil {
				t.Fatal(err)
			}
			if pending == "inner" {
				if _, err := os.Stat(tomb); !os.IsNotExist(err) {
					t.Fatalf("inner removal did not finish: %v", err)
				}
				if _, err := os.Stat(outer); err != nil {
					t.Fatalf("unconfirmed outer removal: %v", err)
				}
			} else if _, err := os.Stat(filepath.Join(tomb, "inner", ".git")); err != nil {
				t.Fatalf("user checkout in moved tree was removed: %v", err)
			}
		})
	}
}
