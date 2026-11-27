//go:build unix

package runtime

import (
	"syscall"
)

func (f *repoFS) identity(path string) (dev, ino uint64, ok bool) {
	info, err := f.lstat(path)
	if err != nil {
		return 0, 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}
