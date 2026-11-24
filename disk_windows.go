//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
	procGetVolumePathNameW  = kernel32.NewProc("GetVolumePathNameW")
)

// diskUsage returns available (to the current user) and total bytes of the volume holding p.
func diskUsage(p string) (free, total uint64) {
	ptr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return 0, 0
	}
	var avail, tot, totFree uint64
	r, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&tot)),
		uintptr(unsafe.Pointer(&totFree)),
	)
	if r == 0 {
		return 0, 0
	}
	return avail, tot
}

// mountPoint returns the volume root holding p (e.g. D:\ or a mounted folder). "" if unknown.
func mountPoint(p string) string {
	ptr, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return ""
	}
	buf := make([]uint16, 1024)
	if r, _, _ := procGetVolumePathNameW.Call(uintptr(unsafe.Pointer(ptr)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf)
}
