package detailprofile

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// rssMethod documents how peakRSSBytes samples are obtained, so a recorded
// result states the measurement it came from rather than leaving readers to
// assume one.
const rssMethod = "50ms samples of /proc/self/status VmRSS; absolute process peak includes Go runtime state retained from earlier samples"

// hostMemoryBytes reports installed memory for runs outside a cgroup.
func hostMemoryBytes() (uint64, error) {
	var info syscall.Sysinfo_t
	if err := syscall.Sysinfo(&info); err != nil {
		return 0, fmt.Errorf("inspect host memory: %w", err)
	}
	totalRAM := widenToUnsigned(info.Totalram)
	memoryUnit := uint64(info.Unit)
	const maxInt64 = uint64(1<<63 - 1)
	if totalRAM == 0 || memoryUnit == 0 || totalRAM > maxInt64/memoryUnit {
		return 0, fmt.Errorf("host memory size is unavailable")
	}
	return totalRAM * memoryUnit, nil
}

func currentRSS() int64 {
	file, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "VmRSS:" {
			value, _ := strconv.ParseInt(fields[1], 10, 64)
			return value * 1024
		}
	}
	return 0
}
