//go:build !unix

package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// tryLock falls back to exclusive file creation on platforms without flock. A
// lock file left behind by a killed process has to be removed by hand, which is
// reported rather than guessed at; background service installation is not
// supported on those platforms in this slice.
func tryLock(path string) (Unlock, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open lock %s: %w", path, err)
	}
	name := file.Name()
	return func() error {
		_ = file.Close()
		return os.Remove(name)
	}, true, nil
}
