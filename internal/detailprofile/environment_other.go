//go:build !linux && !darwin

package detailprofile

import (
	"fmt"
	"runtime"
)

// rssMethod documents how peakRSSBytes samples are obtained, so a recorded
// result states the measurement it came from rather than leaving readers to
// assume one.
const rssMethod = "unavailable"

// hostMemoryBytes, inspectFilesystem and currentRSS have no portable
// implementation. The benchmark records acceptance gates against the machine it
// ran on, so an unmeasurable environment is reported rather than defaulted:
// a zero memory limit or peak RSS would let a regression gate pass vacuously.

func hostMemoryBytes() (uint64, error) {
	return 0, fmt.Errorf("host memory size is unavailable on %s", runtime.GOOS)
}

func inspectFilesystem(string) (filesystemIdentity, error) {
	return filesystemIdentity{}, fmt.Errorf("output filesystem identity is unavailable on %s", runtime.GOOS)
}

func currentRSS() int64 { return 0 }

type filesystemIdentity struct {
	description    string
	device         uint64
	availableBytes uint64
}
