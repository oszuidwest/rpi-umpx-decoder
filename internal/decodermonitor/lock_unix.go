//go:build linux || darwin

package decodermonitor

import (
	"os"
	"syscall"
)

func lockState(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
