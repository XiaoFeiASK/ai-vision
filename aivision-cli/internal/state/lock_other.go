//go:build !unix

package state

import (
	"fmt"
	"os"
	"time"
)

// acquireRecordLock falls back to an atomic lock directory on platforms
// without flock. A stale lock is reclaimed only after its mtime ages out.
func acquireRecordLock(store *recordStore, rel string) (func(), error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := store.root.Mkdir(rel, 0o700)
		if err == nil {
			return func() { _ = store.root.Remove(rel) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if info, statErr := store.root.Stat(rel); statErr == nil && time.Since(info.ModTime()) > 2*time.Minute {
			if store.root.Remove(rel) == nil {
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for ownership record lock %s", rel)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
