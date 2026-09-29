//go:build !unix

package service

// tryLock falls back to a permanent anchor plus an exclusive sentinel on
// platforms without flock. A sentinel left behind by a killed process has to
// be removed by hand, which is reported rather than guessed at; background
// service installation is not supported on those platforms in this slice.
func tryLock(path string) (Unlock, bool, error) {
	return tryPortableLock(path)
}
