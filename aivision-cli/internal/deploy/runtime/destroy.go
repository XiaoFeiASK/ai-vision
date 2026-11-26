package runtime

import (
	"github.com/antgroup/ai-vision/aivision-cli/internal/deploy/components"

	"fmt"
	"os"
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

	var managedRepos []ManagedRepo
	if opts.PurgeRepos {
		var err error
		managedRepos, err = a.purgeRepoTargets()
		if err != nil {
			return err
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
		if err := a.removeManagedRepos(managedRepos); err != nil {
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

// purgeRepoTargets validates every repository path before anything is deleted
// and returns the checkouts aiv itself cloned. Blueprint paths outside
// sourceRoot are rejected; unusable ownership records are skipped so they
// cannot block cleanup of other eligible checkouts.
func (a *App) purgeRepoTargets() ([]ManagedRepo, error) {
	for _, c := range a.BP.Components {
		if c.Repo == nil {
			continue
		}
		dir := a.RepoDir(c)
		if dir == "" {
			continue
		}
		if err := pathguard.EnsureWithin(a.SourceRoot(), dir); err != nil {
			return nil, fmt.Errorf("component %q: refusing --purge-repos: %w", c.Name, err)
		}
	}
	repos, err := a.State.ManagedRepos()
	if err != nil {
		return nil, fmt.Errorf("read recorded repos: %w", err)
	}
	eligible := make([]ManagedRepo, 0, len(repos))
	for _, repo := range repos {
		if err := pathguard.EnsureStrictlyWithin(a.SourceRoot(), repo.Path); err != nil {
			a.Log.Warn("keeping recorded repo %s: %v", repo.Path, err)
			continue
		}
		if repo.ID == "" {
			a.Log.Warn("keeping recorded repo %s: no verified ownership id", repo.Path)
			continue
		}
		eligible = append(eligible, repo)
	}
	return eligible, nil
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
		if _, err := os.Lstat(dir); err != nil {
			if os.IsNotExist(err) {
				if err := a.State.ForgetManagedRepo(dir); err != nil {
					return fmt.Errorf("forget removed repo %s: %w", dir, err)
				}
				continue
			}
			return fmt.Errorf("inspect repo %s: %w", dir, err)
		}
		id, err := repoOwnerID(dir)
		if err != nil {
			a.Log.Warn("keeping repo %s: %v", dir, err)
			continue
		}
		if id != repo.ID {
			a.Log.Warn("keeping repo %s: ownership id does not match the record", dir)
			continue
		}
		a.Log.Info("removing aiv-cloned repo %s", dir)
		if err := os.RemoveAll(dir); err != nil {
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
