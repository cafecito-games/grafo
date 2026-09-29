package detailprofile

import (
	"os"
	"path/filepath"
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

func TestInspectCgroupLimitsRejectsPartialV1Provenance(t *testing.T) {
	root := t.TempDir()
	writeEnvironmentFile(t, filepath.Join(root, "cpu", "cpu.cfs_quota_us"), "800000\n")
	writeEnvironmentFile(t, filepath.Join(root, "cpu", "cpu.cfs_period_us"), "100000\n")

	_, err := inspectCgroupLimits(root)
	if err == nil || !strings.Contains(err.Error(), "memory.limit_in_bytes") {
		t.Fatalf("error = %v", err)
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
