//go:build !windows

package platform

import "golang.org/x/sys/unix"

// Disk returns capacity for the filesystem containing path.
func Disk(path string) (DiskStat, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return DiskStat{}, err
	}
	bs := uint64(s.Bsize)
	return DiskStat{Total: s.Blocks * bs, Free: s.Bavail * bs}, nil
}
