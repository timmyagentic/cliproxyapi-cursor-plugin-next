//go:build windows

package main

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var usageLockFile = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
var usageUnlockFile = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")

func lockUsageJournal(file *os.File) error {
	var overlap syscall.Overlapped
	ok, _, _ := usageLockFile.Call(file.Fd(), 0x00000002|0x00000001, 0, 1, 0, uintptr(unsafe.Pointer(&overlap)))
	if ok == 0 {
		return errors.New("usage journal already owned or unavailable")
	}
	return nil
}

func unlockUsageJournal(file *os.File) {
	var overlap syscall.Overlapped
	_, _, _ = usageUnlockFile.Call(file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlap)))
}
