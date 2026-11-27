//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// acquireRecordLock uses an advisory file lock, which the kernel releases
// automatically when the process exits. A live holder therefore cannot lose
// its lock because of an old timestamp, and there is no stale lock to reclaim.
func acquireRecordLock(store *recordStore, rel string) (func(), error) {
	f, err := store.work.OpenFile(rel, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("timed out waiting for ownership record lock %s", rel)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
