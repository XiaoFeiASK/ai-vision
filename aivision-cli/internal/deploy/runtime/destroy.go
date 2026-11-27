package runtime

import (
	"github.com/antgroup/ai-vision/aivision-cli/internal/deploy/components"

	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	// ConfirmedRepos limits removal to the candidates shown to the user.
	// A nil slice keeps the unattended behaviour (remove every eligible repo).
	ConfirmedRepos []ManagedRepo
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
		if opts.ConfirmedRepos != nil {
			repoPlan = confirmedRepoPlan(repoPlan, opts.ConfirmedRepos)
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
	repos, err := a.PurgeRepoPlan()
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(repos))
	for _, repo := range repos {
		paths = append(paths, repo.Path)
	}
	return paths, nil
}

// PurgeRepoPlan returns the records the user is asked to confirm.
func (a *App) PurgeRepoPlan() ([]ManagedRepo, error) {
	plan, err := a.repoPurgePlan()
	if err != nil {
		return nil, err
	}
	out := make([]ManagedRepo, 0, len(plan.candidates))
	out = append(out, plan.candidates...)
	return out, nil
}

func confirmedRepoPlan(plan repoPurgePlan, confirmed []ManagedRepo) repoPurgePlan {
	want := make(map[string]bool, len(confirmed))
	wantPath := make(map[string]bool, len(confirmed))
	for _, repo := range confirmed {
		want[repoKey(repo)] = true
		wantPath[repo.Path] = true
	}
	out := repoPurgePlan{kept: plan.kept}
	matched := map[string]bool{}
	orphanPath := map[string]bool{}
	for _, repo := range plan.candidates {
		if want[repoKey(repo)] {
			out.candidates = append(out.candidates, repo)
			matched[repoKey(repo)] = true
		}
	}
	for _, dir := range plan.orphans {
		if wantPath[dir] {
			out.orphans = append(out.orphans, dir)
			orphanPath[dir] = true
		}
	}
	for _, repo := range confirmed {
		if !matched[repoKey(repo)] && !orphanPath[repo.Path] {
			out.kept = append(out.kept, repo.Path+" (confirmed removal is no longer eligible)")
		}
	}
	return out
}

func repoKey(repo ManagedRepo) string {
	return repo.Path + "\x00" + repo.ID
}

// repoPurgePlan validates every repository path before anything is deleted.
// Blueprint paths outside sourceRoot are rejected; unusable records are kept
// so they cannot block cleanup of other eligible checkouts.
func (a *App) repoPurgePlan() (repoPurgePlan, error) {
	var plan repoPurgePlan
	for _, c := range a.BP.Components {
		if c.Repo == nil || !c.IsEnabled() {
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
	pending := make(map[string]ManagedRepo)
	for _, repo := range repos {
		if repo.Pending {
			pending[repo.Path] = repo
		}
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
		if repo.Pending {
			if err := a.addPendingCandidate(&plan, repo, tombstone); err != nil {
				return plan, err
			}
			continue
		}
		if pendingRepo, ok := pending[repo.Path]; ok {
			plan.kept = append(plan.kept, repo.Path+" (pending removal for "+pendingRepo.ID+" is still in progress)")
			continue
		}
		if _, err := os.Lstat(repo.Path); err != nil {
			if !os.IsNotExist(err) {
				return plan, fmt.Errorf("inspect repo %s: %w", repo.Path, err)
			}
			// A directory at the tombstone path is only owned cleanup when the
			// record says a removal is pending; otherwise leave it alone.
			plan.orphans = append(plan.orphans, repo.Path)
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

func (a *App) addPendingCandidate(plan *repoPurgePlan, repo ManagedRepo, tombstone string) error {
	if _, err := os.Lstat(tombstone); err == nil {
		if !purgeMarkerOwnsTombstone(tombstone, repo) {
			plan.kept = append(plan.kept, repo.Path+" (pending removal path is not owned by this record)")
			return nil
		}
		plan.candidates = append(plan.candidates, repo)
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect pending removal %s: %w", tombstone, err)
	}
	if _, err := os.Lstat(repo.Path); err == nil {
		if err := verifyRepoOwner(repo.Path, repo.ID); err != nil {
			plan.kept = append(plan.kept, repo.Path+" (pending removal cannot verify the checkout)")
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect repo %s: %w", repo.Path, err)
	}
	plan.candidates = append(plan.candidates, repo)
	return nil
}

// purgeTombstone names the sibling used to make removal restartable: once
// ownership is verified the checkout is renamed here before deletion, so an
// interrupted recursive delete can be resumed on the next invocation.
func (a *App) purgeTombstone(repo ManagedRepo) string {
	// Hash the full target so a crafted id cannot add path components and a
	// long checkout basename cannot exceed the filesystem name limit.
	sum := sha256.Sum256([]byte(repo.Path + "\x00" + repo.ID))
	suffix := hex.EncodeToString(sum[:8])
	return filepath.Join(filepath.Dir(repo.Path), ".aiv-purge-"+suffix)
}

func purgeMarkerPath(tombstone string) string { return tombstone + ".owner" }

type purgeMarkerRecord struct {
	Path        string `json:"path"`
	ID          string `json:"id"`
	HasIdentity bool   `json:"hasIdentity,omitempty"`
	Dev         uint64 `json:"dev,omitempty"`
	Ino         uint64 `json:"ino,omitempty"`
}

func writePurgeMarker(tombstone string, repo ManagedRepo) error {
	marker := purgeMarkerRecord{Path: repo.Path, ID: repo.ID}
	if dev, ino, ok := fileIdentity(repo.Path); ok {
		marker.HasIdentity = true
		marker.Dev = dev
		marker.Ino = ino
	}
	b, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	path := purgeMarkerPath(tombstone)
	f, err := os.CreateTemp(filepath.Dir(path), ".aiv-owner-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
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
	return os.Rename(tmp, path)
}

func readPurgeMarker(tombstone string) (purgeMarkerRecord, error) {
	b, err := os.ReadFile(purgeMarkerPath(tombstone))
	if err != nil {
		return purgeMarkerRecord{}, err
	}
	var marker purgeMarkerRecord
	if err := json.Unmarshal(b, &marker); err != nil {
		return purgeMarkerRecord{}, err
	}
	return marker, nil
}

func purgeMarkerMatches(tombstone string, repo ManagedRepo) bool {
	marker, err := readPurgeMarker(tombstone)
	return err == nil && marker.Path == repo.Path && marker.ID == repo.ID
}

// purgeMarkerOwnsTombstone verifies that the directory at the tombstone path
// is the object that was renamed, not merely that a removal was once planned.
func purgeMarkerOwnsTombstone(tombstone string, repo ManagedRepo) bool {
	marker, err := readPurgeMarker(tombstone)
	if err != nil || marker.Path != repo.Path || marker.ID != repo.ID {
		return false
	}
	if marker.HasIdentity {
		dev, ino, ok := fileIdentity(tombstone)
		return ok && dev == marker.Dev && ino == marker.Ino
	}
	// Without a persisted identity, only trust the tombstone when the checkout
	// is no longer at its original path, so the rename must have happened.
	if _, err := os.Lstat(repo.Path); err == nil || !os.IsNotExist(err) {
		return false
	}
	return true
}

// beginPendingRemoval records the intent and ownership of a tombstone before
// the checkout is renamed, so a resumed cleanup never deletes an unrelated
// directory that happens to use the same name.
func (a *App) beginPendingRemoval(repo ManagedRepo, tombstone string) error {
	if err := a.State.SetRepoPending(repo.Path, repo.ID, true); err != nil {
		return fmt.Errorf("mark repo %s pending: %w", repo.Path, err)
	}
	if err := writePurgeMarker(tombstone, repo); err != nil {
		_ = a.State.SetRepoPending(repo.Path, repo.ID, false)
		return fmt.Errorf("mark pending removal %s: %w", tombstone, err)
	}
	if err := os.Rename(repo.Path, tombstone); err != nil {
		_ = os.Remove(purgeMarkerPath(tombstone))
		_ = a.State.SetRepoPending(repo.Path, repo.ID, false)
		return fmt.Errorf("remove repo %s: %w", repo.Path, err)
	}
	return nil
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
		if repo.Pending {
			if err := a.finishPendingRemoval(repo, tombstone); err != nil {
				return err
			}
			continue
		}
		if _, err := os.Lstat(dir); err != nil {
			if os.IsNotExist(err) {
				if err := a.State.ForgetManagedRepoID(dir, repo.ID); err != nil {
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
			a.Log.Warn("keeping repo %s: removal path %s already exists but the record is not pending", dir, tombstone)
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect pending removal %s: %w", tombstone, err)
		}
		if _, err := os.Lstat(purgeMarkerPath(tombstone)); err == nil {
			if !purgeMarkerMatches(tombstone, repo) {
				a.Log.Warn("keeping repo %s: pending-removal marker %s belongs to another record", dir, purgeMarkerPath(tombstone))
				continue
			}
			if err := os.Remove(purgeMarkerPath(tombstone)); err != nil {
				return fmt.Errorf("clear pending-removal marker %s: %w", purgeMarkerPath(tombstone), err)
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect pending-removal marker %s: %w", purgeMarkerPath(tombstone), err)
		}
		a.Log.Info("removing aiv-cloned repo %s", dir)
		if err := a.beginPendingRemoval(repo, tombstone); err != nil {
			return err
		}
		if err := os.RemoveAll(tombstone); err != nil {
			return fmt.Errorf("remove repo %s: %w", dir, err)
		}
		_ = os.Remove(purgeMarkerPath(tombstone))
		if err := a.State.ForgetManagedRepoID(dir, repo.ID); err != nil {
			return fmt.Errorf("forget removed repo %s: %w", dir, err)
		}
	}
	return nil
}

func (a *App) finishPendingRemoval(repo ManagedRepo, tombstone string) error {
	dir := repo.Path
	if _, err := os.Lstat(tombstone); err == nil {
		if !purgeMarkerOwnsTombstone(tombstone, repo) {
			a.Log.Warn("keeping repo %s: pending removal %s is not the renamed checkout", dir, tombstone)
			return nil
		}
		a.Log.Info("finishing interrupted removal %s", dir)
		if err := os.RemoveAll(tombstone); err != nil {
			return fmt.Errorf("remove repo %s: %w", dir, err)
		}
		_ = os.Remove(purgeMarkerPath(tombstone))
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect pending removal %s: %w", tombstone, err)
	} else if _, err := os.Lstat(dir); err == nil {
		if err := verifyRepoOwner(dir, repo.ID); err != nil {
			a.Log.Warn("keeping repo %s: pending removal cannot verify ownership: %v", dir, err)
			return nil
		}
		a.Log.Info("resuming removal of %s", dir)
		if err := a.beginPendingRemoval(repo, tombstone); err != nil {
			return err
		}
		if err := os.RemoveAll(tombstone); err != nil {
			return fmt.Errorf("remove repo %s: %w", dir, err)
		}
		_ = os.Remove(purgeMarkerPath(tombstone))
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect repo %s: %w", dir, err)
	}
	_ = os.Remove(purgeMarkerPath(tombstone))
	if err := a.State.ForgetManagedRepoID(dir, repo.ID); err != nil {
		return fmt.Errorf("forget removed repo %s: %w", dir, err)
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
