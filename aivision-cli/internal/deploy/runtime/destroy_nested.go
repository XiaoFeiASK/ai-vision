package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/antgroup/ai-vision/aivision-cli/internal/pathguard"
)

// nestedCheckout inspects only the checkout roots declared in the blueprint.
// tree is the current location of the outer checkout, including after a rename.
// An eligible inner clone may be shown in the same preview as its parent.
func (a *App) nestedCheckout(fs *repoFS, outer, tree string, eligible map[string]bool, records []ManagedRepo) (string, error) {
	for _, component := range a.BP.Components {
		child := a.RepoDir(component)
		if child == "" {
			continue
		}
		rel, err := nestedCheckoutRel(outer, tree, child)
		if err != nil {
			return "", err
		}
		if rel == "" || (tree == outer && eligible[child]) {
			continue
		}
		if _, err := fs.lstat(filepath.Join(tree, rel)); err == nil {
			return child, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		// A nested removal may already have moved the checkout to its sibling
		// tombstone. Let that operation finish before removing the parent.
		if records == nil {
			records, err = a.State.ManagedRepos()
			if err != nil {
				return "", err
			}
		}
		for _, record := range records {
			if record.Pending && record.Path == child {
				return child, nil
			}
		}
	}
	return "", nil
}

func nestedCheckoutRel(parent, tree, child string) (string, error) {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", nil
	}
	if filepath.IsLocal(rel) {
		return rel, nil
	}
	rel, inside, err := pathguard.RelativeWithin(parent, child)
	if err != nil || rel == "." {
		return "", err
	}
	if inside || tree == parent {
		return rel, nil
	}
	// The outer name may already be gone. Its parent still identifies the
	// volume and the location where an alternate spelling used to resolve.
	base := filepath.Dir(parent)
	rel, _, err = pathguard.RelativeWithin(base, child)
	entry, suffix, nested := strings.Cut(rel, string(filepath.Separator))
	if err != nil || !nested || !strings.EqualFold(entry, filepath.Base(parent)) {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(base, entry)); os.IsNotExist(err) {
		return suffix, nil
	} else {
		return "", err
	}
}

func (a *App) verifyNestedCheckouts(fs *repoFS, parent, tree string) error {
	child, err := a.nestedCheckout(fs, parent, tree, nil, nil)
	if err != nil {
		return err
	}
	if child != "" {
		return fmt.Errorf("keeping %s: contains separate checkout %s", tree, child)
	}
	return nil
}
