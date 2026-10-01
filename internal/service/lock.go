package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Unlock releases a lock acquired by Lock.
type Unlock func() error

// lockPoll bounds how often an unavailable lock is retried.
var lockPoll = 20 * time.Millisecond

// ErrLockBusy reports that a competing grafo process still held the lock when
// the wait elapsed. It is ordinary contention rather than a broken lock, so
// callers can match on it and render guidance in their own vocabulary.
var ErrLockBusy = errors.New("locked by another grafo process")

// Lock takes an exclusive advisory cross-process lock on path, creating the file
// if it is missing, and waits up to wait for a competing holder to release it.
//
// Locks are advisory but kernel-backed on Unix, which is what makes a killed
// supervisor safe: the operating system drops the lock when the process dies, so
// no stale marker has to be reasoned about and no manual cleanup is needed.
func Lock(path string, wait time.Duration) (Unlock, error) {
	if directory := filepath.Dir(path); directory != "" {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create lock directory %s: %w", directory, err)
		}
	}
	deadline := time.Now().Add(wait)
	for {
		unlock, acquired, err := tryLock(path)
		if err != nil {
			return nil, err
		}
		if acquired {
			return unlock, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%s is %w", path, ErrLockBusy)
		}
		time.Sleep(lockPoll)
	}
}

// IndexLock serializes writers of one branch index. Both the background
// supervisor and the foreground `grafo index`/`grafo watch` commands take it, so
// a daemon run and a foreground run can never write the same index concurrently.
// The lock lives beside the index inside .grafo, which is never committed.
func IndexLock(indexPath string, wait time.Duration) (Unlock, error) {
	return Lock(indexPath+".lock", wait)
}

// TryIndexLock attempts to serialize a branch-index mutation without waiting.
// A false acquired result is ordinary contention, not an operation error. The
// zero-byte lock anchor intentionally remains after unlock so waiters can never
// race a replacement inode.
func TryIndexLock(indexPath string) (unlock Unlock, acquired bool, err error) {
	path := indexPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("create lock directory %s: %w", filepath.Dir(path), err)
	}
	return tryLock(path)
}

// tryPortableLock is the non-flock fallback. The stable anchor remains at
// path; exclusive creation and removal use a separate transient sentinel so
// unlocking can never replace the inode named by the public lock anchor.
func tryPortableLock(path string) (Unlock, bool, error) {
	anchor, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock anchor %s: %w", path, err)
	}
	if err := anchor.Close(); err != nil {
		return nil, false, fmt.Errorf("close lock anchor %s: %w", path, err)
	}
	heldPath := path + ".held"
	file, err := os.OpenFile(heldPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open lock sentinel %s: %w", heldPath, err)
	}
	return func() error {
		return errors.Join(file.Close(), os.Remove(heldPath))
	}, true, nil
}
