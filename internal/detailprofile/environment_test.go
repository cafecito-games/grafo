package detailprofile

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInspectCgroupLimitsFallsBackToV1(t *testing.T) {
	root := t.TempDir()
	writeEnvironmentFile(t, filepath.Join(root, "cpu", "cpu.cfs_quota_us"), "800000\n")
	writeEnvironmentFile(t, filepath.Join(root, "cpu", "cpu.cfs_period_us"), "100000\n")
	writeEnvironmentFile(t, filepath.Join(root, "memory", "memory.limit_in_bytes"), "21474836480\n")

	limits, err := inspectCgroupLimits(root)
	if err != nil {
		t.Fatal(err)
	}
	if limits.cpuQuota != "v1:800000 100000" || limits.effectiveCPUs != 8 || limits.memoryMaxBytes != 21474836480 {
		t.Fatalf("limits = %#v", limits)
	}
}

func TestInspectCgroupLimitsV2(t *testing.T) {
	tests := []struct {
		name          string
		cpu           string
		memory        string
		effectiveCPUs int64
		memoryBytes   int64
	}{
		{name: "numeric", cpu: "800000 100000", memory: "21474836480", effectiveCPUs: 8, memoryBytes: 21474836480},
		{name: "unlimited CPU", cpu: "max 100000", memory: "21474836480", effectiveCPUs: 0, memoryBytes: 21474836480},
		{name: "unlimited memory", cpu: "800000 100000", memory: "max", effectiveCPUs: 8, memoryBytes: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeEnvironmentFile(t, filepath.Join(root, "cpu.max"), test.cpu+"\n")
			writeEnvironmentFile(t, filepath.Join(root, "memory.max"), test.memory+"\n")

			limits, err := inspectCgroupLimits(root)
			if err != nil {
				t.Fatal(err)
			}
			if limits.cpuQuota != test.cpu || limits.effectiveCPUs != test.effectiveCPUs || limits.memoryMaxBytes != test.memoryBytes {
				t.Fatalf("limits = %#v", limits)
			}
		})
	}
}

func TestInspectCgroupLimitsRejectsPartialV2Provenance(t *testing.T) {
	root := t.TempDir()
	writeEnvironmentFile(t, filepath.Join(root, "cpu.max"), "800000 100000\n")

	_, err := inspectCgroupLimits(root)
	if err == nil || !strings.Contains(err.Error(), "read cgroup v2 limits") {
		t.Fatalf("error = %v", err)
	}
}

func TestInspectCgroupLimitsRejectsPartialV1Provenance(t *testing.T) {
	root := t.TempDir()
	writeEnvironmentFile(t, filepath.Join(root, "cpu", "cpu.cfs_quota_us"), "800000\n")
	writeEnvironmentFile(t, filepath.Join(root, "cpu", "cpu.cfs_period_us"), "100000\n")

	_, err := inspectCgroupLimits(root)
	if err == nil || !strings.Contains(err.Error(), "memory.limit_in_bytes") {
		t.Fatalf("error = %v", err)
	}
}

func TestInspectCgroupLimitsDoesNotHideAlternateV1CPUController(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvironmentFile(t, filepath.Join(root, "cpu,cpuacct", "cpu.cfs_quota_us"), "100000\n")
	writeEnvironmentFile(t, filepath.Join(root, "cpu,cpuacct", "cpu.cfs_period_us"), "100000\n")

	_, err := inspectCgroupLimits(root)
	if err == nil || !strings.Contains(err.Error(), "CPU controller present: true") {
		t.Fatalf("error = %v", err)
	}
}

func TestInspectCgroupLimitsFallsBackToHostWhenCgroupIsAbsent(t *testing.T) {
	limits, err := inspectCgroupLimits(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wantQuota := fmt.Sprintf("host:logical-cpus=%d", runtime.NumCPU())
	if limits.cpuQuota != wantQuota || limits.effectiveCPUs != int64(runtime.NumCPU()) || limits.memoryMaxBytes <= 0 {
		t.Fatalf("limits = %#v", limits)
	}
}

func writeEnvironmentFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
