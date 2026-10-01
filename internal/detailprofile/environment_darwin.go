package detailprofile

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// rssMethod documents how peakRSSBytes samples are obtained, so a recorded
// result states the measurement it came from rather than leaving readers to
// assume one. Darwin has no /proc, and getrusage already reports a
// process-lifetime peak, so sampling it only observes that peak sooner.
const rssMethod = "50ms samples of getrusage ru_maxrss; absolute process peak includes Go runtime state retained from earlier samples"

// hostMemoryBytes reports installed memory. Darwin has no cgroups, so this is
// always the memory figure a run is measured against.
func hostMemoryBytes() (uint64, error) {
	totalRAM, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, fmt.Errorf("inspect host memory: %w", err)
	}
	const maxInt64 = uint64(1<<63 - 1)
	if totalRAM == 0 || totalRAM > maxInt64 {
		return 0, fmt.Errorf("host memory size is unavailable")
	}
	return totalRAM, nil
}

func currentRSS() int64 {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	// Darwin reports ru_maxrss in bytes, unlike the kilobytes Linux reports.
	return usage.Maxrss
}
