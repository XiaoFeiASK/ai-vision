package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const repoOwnerMarker = ".git/aivision-clone.json"

type repoOwner struct {
	ID    string `json:"id"`
	Scope string `json:"scope,omitempty"`
}

// newRepoOwnerID returns a random identity for a checkout cloned by aiv.
func newRepoOwnerID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeRepoOwner records ownership inside .git so it never appears as an
// untracked file in the checkout.
func writeRepoOwner(dir, id string, scope ...string) error {
	gitDir := filepath.Join(dir, ".git")
	if info, err := os.Stat(gitDir); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a normal git checkout", dir)
	}
	owner := repoOwner{ID: id}
	if len(scope) > 0 {
		owner.Scope = scope[0]
	}
	b, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, repoOwnerMarker), b, 0o644)
}

func readRepoOwner(dir string) (repoOwner, error) {
	b, err := os.ReadFile(filepath.Join(dir, repoOwnerMarker))
	if err != nil {
		return repoOwner{}, err
	}
	var owner repoOwner
	if err := json.Unmarshal(b, &owner); err != nil {
		return repoOwner{}, fmt.Errorf("parse %s: %w", filepath.Join(dir, repoOwnerMarker), err)
	}
	if owner.ID == "" {
		return repoOwner{}, fmt.Errorf("%s has no ownership id", filepath.Join(dir, repoOwnerMarker))
	}
	return owner, nil
}

// repoOwnerID reads the ownership marker written at clone time.
func repoOwnerID(dir string) (string, error) {
	owner, err := readRepoOwner(dir)
	if err != nil {
		return "", err
	}
	return owner.ID, nil
}
