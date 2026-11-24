//go:build !windows

package main

import (
	"path/filepath"
	"syscall"
)

// diskUsage returns available (to the current user) and total bytes of the filesystem holding p.
func diskUsage(p string) (free, total uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, 0
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs
}

// mountPoint returns the mount point of the filesystem holding p (e.g. /mnt/nas for
// /mnt/nas/a/b): the topmost ancestor on the same device. "" if unknown.
func mountPoint(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	p, _ = filepath.Abs(p)
	var st syscall.Stat_t
	if syscall.Stat(p, &st) != nil {
		return ""
	}
	for {
		parent := filepath.Dir(p)
		var ps syscall.Stat_t
		if parent == p || syscall.Stat(parent, &ps) != nil || ps.Dev != st.Dev {
			return p
		}
		p = parent
	}
}
