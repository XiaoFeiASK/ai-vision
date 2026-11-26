package runtime

import (
	"github.com/antgroup/ai-vision/aivision-cli/internal/deploy/components"

	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/antgroup/ai-vision/aivision-cli/internal/pathguard"
)

// DestroyOpts controls teardown scope.
type DestroyOpts struct {
	Purge      bool // remove named volumes (datastores) + the whole state dir
	PurgeRepos bool // also delete cloned source repos
}

// Destroy tears the stack down in reverse dependency order. By default it stops
// processes and removes containers but preserves data volumes, cloned repos and
// state so a later cast comes back up fast. --purge additionally removes named
// volumes (wiping the OceanBase datastore) and the state directory.
func (a *App) Destroy(opts DestroyOpts) error {
	if err := a.checkDeploymentDriver(); err != nil {
		return err
	}
	if opts.Purge {
		if err := a.State.VerifyPurgeTarget(); err != nil {
			return fmt.Errorf("remove state: %w", err)
		}
	}
	a.Log.Step("destroy: tearing down namespace %s (purge=%v)", a.BP.Metadata.Namespace, opts.Purge)

	var repoPlan repoPurgePlan
	if opts.PurgeRepos {
		var err error
		repoPlan, err = a.repoPurgePlan()
		if err != nil {
			return err
		}
		for _, kept := range repoPlan.kept {
			a.Log.Warn("keeping recorded repo %s", kept)
		}
		for _, dir := range repoPlan.orphans {
			if err := a.State.ForgetManagedRepo(dir); err != nil {
				return fmt.Errorf("forget removed repo %s: %w", dir, err)
			}
		}
	}

	comps := a.BP.OrderedComponents()
	// reverse order: dependents first
	rev := make([]Component, 0, len(comps))
	for i := len(comps) - 1; i >= 0; i-- {
		rev = append(rev, comps[i])
	}

	for _, c := range rev {
		vars := a.ComponentVars(c)
		if len(c.Teardown) > 0 {
			a.Log.Info("teardown hooks for %s", components.Canonical(c.Name))
			_ = a.RunSteps(c, c.Teardown, vars, "teardown")
		}
		switch c.Kind {
		case "process":
			pid := a.State.ReadPid(c.Name)
			if pid > 0 && PidAlive(pid) {
				a.Log.Info("stopping process %s (pid %d)", components.Canonical(c.Name), pid)
				if err := terminateTree(pid); err != nil {
					a.Log.Warn("terminate %s: %v", components.Canonical(c.Name), err)
				}
			} else {
				a.Log.Info("process %s not running", components.Canonical(c.Name))
			}
			a.State.RemovePid(c.Name)
		case "container":
			name := a.ContainerName(c)
			exists := a.Docker.ContainerExists(a.Ctx, name)
			if a.BP.Metadata.Product == "aivision" {
				var err error
				exists, err = a.productContainerExists(a.Ctx, name)
				if err != nil {
					return err
				}
			}
			if exists {
				a.Log.Info("removing %s container", components.Canonical(c.Name))
				if err := a.removeContainerWithChecks(c, true); err != nil {
					if a.managedDatastore(c) || a.BP.Metadata.Product == "aivision" {
						return fmt.Errorf("stop %s: %w", components.Canonical(c.Name), err)
					}
					a.Log.Warn("remove container %s: %v", name, err)
				}
			} else {
				a.Log.Info("%s container absent", components.Canonical(c.Name))
			}
			if opts.Purge {
				for _, v := range c.Volumes {
					vol := expand(v, vars)
					if idx := strings.IndexByte(vol, ':'); idx > 0 {
						vol = vol[:idx]
					}
					if vol == "" {
						continue
					}
					a.Log.Info("removing volume %s", vol)
					_, _ = a.Docker.Run(a.Ctx, false, "volume", "rm", "-f", vol)
				}
			}
		}
	}

	if opts.PurgeRepos {
		if err := a.removeManagedRepos(repoPlan.candidates); err != nil {
			return err
		}
	}
	if opts.Purge {
		a.Log.Info("purging state dir %s", a.State.Root)
		if err := a.State.RemoveAll(); err != nil {
			return fmt.Errorf("remove state: %w", err)
		}
	}
	a.Log.OK("destroy complete")
	return nil
}

type repoPurgePlan struct {
	candidates []ManagedRepo
	orphans    []string
	kept       []string
}

// PurgeRepoPreview lists the checkouts this invocation can actually remove.
// It applies the same eligibility and ownership checks as Destroy; removal
// re-verifies ownership again before deleting.
func (a *App) PurgeRepoPreview() ([]string, error) {
	plan, err := a.repoPurgePlan()
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(plan.candidates))
	for _, repo := range plan.candidates {
		paths = append(paths, repo.Path)
	}
	return paths, nil
}

// repoPurgePlan validates every repository path before anything is deleted.
// Blueprint paths outside sourceRoot are rejected; unusable records are kept
// so they cannot block cleanup of other eligible checkouts.
func (a *App) repoPurgePlan() (repoPurgePlan, error) {
	var plan repoPurgePlan
	for _, c := range a.BP.Components {
		if c.Repo == nil {
			continue
		}
		dir := a.RepoDir(c)
		if dir == "" {
			continue
		}
		if err := pathguard.EnsureWithin(a.SourceRoot(), dir); err != nil {
			return plan, fmt.Errorf("component %q: refusing --purge-repos: %w", c.Name, err)
		}
	}
	if err := a.State.VerifyRecordTargets(); err != nil {
		return plan, err
	}
	repos, err := a.State.ManagedRepos()
	if err != nil {
		return plan, fmt.Errorf("read recorded repos: %w", err)
	}
	for _, repo := range repos {
		if err := pathguard.EnsureStrictlyWithin(a.SourceRoot(), repo.Path); err != nil {
			plan.kept = append(plan.kept, repo.Path+" ("+err.Error()+")")
			continue
		}
		if repo.ID == "" {
			plan.kept = append(plan.kept, repo.Path+" (no verified ownership id)")
			continue
		}
		tombstone := a.purgeTombstone(repo)
		if err := pathguard.EnsureStrictlyWithin(a.SourceRoot(), tombstone); err != nil {
			return plan, fmt.Errorf("invalid pending removal for %s: %w", repo.Path, err)
		}
		if _, err := os.Lstat(repo.Path); err != nil {
			if !os.IsNotExist(err) {
				return plan, fmt.Errorf("inspect repo %s: %w", repo.Path, err)
			}
			if _, err := os.Lstat(tombstone); err == nil {
				plan.candidates = append(plan.candidates, repo)
			} else if os.IsNotExist(err) {
				plan.orphans = append(plan.orphans, repo.Path)
			} else {
				return plan, fmt.Errorf("inspect pending removal %s: %w", tombstone, err)
			}
			continue
		}
		if err := verifyRepoOwner(repo.Path, repo.ID); err != nil {
			plan.kept = append(plan.kept, repo.Path+" ("+err.Error()+")")
			continue
		}
		plan.candidates = append(plan.candidates, repo)
	}
	return plan, nil
}

// purgeTombstone names the sibling used to make removal restartable: once
// ownership is verified the checkout is renamed here before deletion, so an
// interrupted recursive delete can be resumed on the next invocation.
func (a *App) purgeTombstone(repo ManagedRepo) string {
	// Hash the record-supplied id so a crafted value cannot add path
	// separators or ".." components to the tombstone path.
	sum := sha256.Sum256([]byte(repo.ID))
	suffix := hex.EncodeToString(sum[:8])
	return filepath.Join(filepath.Dir(repo.Path), "."+filepath.Base(repo.Path)+".aiv-purge-"+suffix)
}

func verifyRepoOwner(dir, want string) error {
	id, err := repoOwnerID(dir)
	if err != nil {
		return err
	}
	if id != want {
		return fmt.Errorf("ownership id does not match the record")
	}
	return nil
}

// removeManagedRepos deletes only checkouts aiv cloned. Pre-existing local
// workspaces are never removed, even when they match a blueprint repo.path,
// because the ownership marker must still match the recorded id.
func (a *App) removeManagedRepos(repos []ManagedRepo) error {
	if len(repos) == 0 {
		a.Log.Info("no aiv-cloned repos recorded; pre-existing checkouts are kept")
		return nil
	}
	for _, repo := range repos {
		dir := repo.Path
		if err := pathguard.EnsureStrictlyWithin(a.SourceRoot(), dir); err != nil {
			return fmt.Errorf("refusing to delete repo: %w", err)
		}
		tombstone := a.purgeTombstone(repo)
		if err := pathguard.EnsureStrictlyWithin(a.SourceRoot(), tombstone); err != nil {
			return fmt.Errorf("refusing to delete repo %s: %w", dir, err)
		}
		if _, err := os.Lstat(dir); err != nil {
			if os.IsNotExist(err) {
				if _, err := os.Lstat(tombstone); err == nil {
					a.Log.Info("finishing interrupted removal %s", dir)
					if err := os.RemoveAll(tombstone); err != nil {
						return fmt.Errorf("remove repo %s: %w", dir, err)
					}
				} else if !os.IsNotExist(err) {
					return fmt.Errorf("inspect pending removal %s: %w", tombstone, err)
				}
				if err := a.State.ForgetManagedRepo(dir); err != nil {
					return fmt.Errorf("forget removed repo %s: %w", dir, err)
				}
				continue
			}
			return fmt.Errorf("inspect repo %s: %w", dir, err)
		}
		if err := verifyRepoOwner(dir, repo.ID); err != nil {
			a.Log.Warn("keeping repo %s: %v", dir, err)
			continue
		}
		if _, err := os.Lstat(tombstone); err == nil {
			if err := os.RemoveAll(tombstone); err != nil {
				return fmt.Errorf("clear pending removal %s: %w", tombstone, err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect pending removal %s: %w", tombstone, err)
		}
		a.Log.Info("removing aiv-cloned repo %s", dir)
		if err := os.Rename(dir, tombstone); err != nil {
			return fmt.Errorf("remove repo %s: %w", dir, err)
		}
		if err := os.RemoveAll(tombstone); err != nil {
			return fmt.Errorf("remove repo %s: %w", dir, err)
		}
		if err := a.State.ForgetManagedRepo(dir); err != nil {
			return fmt.Errorf("forget removed repo %s: %w", dir, err)
		}
	}
	return nil
}

// Redeploy is destroy (non-purge) followed by a full cast.
func (a *App) Redeploy(runGauge, runForge bool) error {
	if err := a.Destroy(DestroyOpts{}); err != nil {
		return fmt.Errorf("redeploy: %w", err)
	}
	return a.Cast(runGauge, runForge)
}
