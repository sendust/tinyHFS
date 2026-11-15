//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var procGetDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

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
