//go:build !unix

package runtime

// identity is unavailable on this platform; callers fall back to checking
// whether the original checkout path still exists.
func (f *repoFS) identity(path string) (dev, ino uint64, ok bool) {
	return 0, 0, false
}
