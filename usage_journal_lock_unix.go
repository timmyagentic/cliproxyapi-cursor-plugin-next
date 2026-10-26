//go:build darwin || linux

package main

import (
	"os"
	"syscall"
)

func lockUsageJournal(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockUsageJournal(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
