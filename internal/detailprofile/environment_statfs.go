//go:build linux || darwin

package detailprofile

import (
	"fmt"
	"os"
	"syscall"
)

// filesystemIdentity records which filesystem backs the benchmark output
// directory, so a result can be compared only against results produced on
// comparable storage.
type filesystemIdentity struct {
	description    string
	device         uint64
	availableBytes uint64
}

// inspectFilesystem reports the identity of the filesystem holding path. The
// statfs and stat field widths differ between Linux and Darwin, so the raw
// values are widened here and the reported shape stays platform independent.
func inspectFilesystem(path string) (filesystemIdentity, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return filesystemIdentity{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return filesystemIdentity{}, err
	}
	statInfo, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return filesystemIdentity{}, fmt.Errorf("output filesystem device identity is unavailable")
	}
	blockSize := widenToUnsigned(stat.Bsize)
	return filesystemIdentity{
		description:    fmt.Sprintf("type=0x%x block_size=%d", widenToUnsigned(stat.Type), blockSize),
		device:         widenToUnsigned(statInfo.Dev),
		availableBytes: widenToUnsigned(stat.Bavail) * blockSize,
	}, nil
}

// widenToUnsigned normalizes the platform-dependent widths and signedness of
// statfs and stat fields. Every field it is applied to counts bytes, blocks, or
// identities and is therefore never negative.
func widenToUnsigned[T ~int32 | ~int64 | ~uint32 | ~uint64](value T) uint64 {
	return uint64(value)
}
