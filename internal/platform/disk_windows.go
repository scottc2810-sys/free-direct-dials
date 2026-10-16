//go:build windows

package platform

import "golang.org/x/sys/windows"

// Disk returns capacity for the volume containing path (dev machines only).
func Disk(path string) (DiskStat, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return DiskStat{}, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return DiskStat{}, err
	}
	return DiskStat{Total: total, Free: free}, nil
}
