package service

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Unlock releases a lock acquired by Lock.
type Unlock func() error

// lockPoll bounds how often an unavailable lock is retried.
var lockPoll = 20 * time.Millisecond

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
			return nil, fmt.Errorf("%s is locked by another grafo process", path)
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
