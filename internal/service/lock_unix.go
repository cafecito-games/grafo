//go:build unix

package service

import (
	"fmt"
	"os"
	"syscall"
)

// tryLock takes a non-blocking flock. The kernel releases it when the holding
// process exits, however it exits.
func tryLock(path string) (Unlock, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() error {
		defer func() { _ = file.Close() }()
		return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	}, true, nil
}
