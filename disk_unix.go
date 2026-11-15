//go:build !windows

package main

import "syscall"

// diskUsage returns available (to the current user) and total bytes of the filesystem holding p.
func diskUsage(p string) (free, total uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs
}
