//go:build !unix

package runtime

// fileIdentity is unavailable on this platform; callers fall back to checking
// whether the original checkout path still exists.
func fileIdentity(path string) (dev, ino uint64, ok bool) {
	return 0, 0, false
}
