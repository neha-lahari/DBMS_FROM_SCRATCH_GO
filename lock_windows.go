//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32   = syscall.NewLazyDLL("kernel32.dll")
	lockFileEx = kernel32.NewProc("LockFileEx")
)

func lockDatabaseFile(f *os.File) error {
	const lockfileExclusiveLock = 0x00000002
	const lockfileFailImmediately = 0x00000001
	type overlapped struct {
		Internal     uintptr
		InternalHigh uintptr
		Offset       uint32
		OffsetHigh   uint32
		HEvent       uintptr
	}
	var ov overlapped
	r, _, e := lockFileEx.Call(
		f.Fd(),
		lockfileExclusiveLock|lockfileFailImmediately,
		0, 1, 0,
		uintptr(unsafe.Pointer(&ov)),
	)
	if r == 0 {
		return e
	}
	return nil
}
