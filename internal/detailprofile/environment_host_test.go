//go:build linux || darwin

package detailprofile

import (
	"strings"
	"testing"
)

func TestInspectFilesystemDescribesOutputDirectory(t *testing.T) {
	identity, err := inspectFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(identity.description, "block_size=") {
		t.Errorf("description = %q, want a block size", identity.description)
	}
	if identity.device == 0 {
		t.Error("device = 0, want the device backing the output directory")
	}
	if identity.availableBytes == 0 {
		t.Error("availableBytes = 0, want the free space on the output filesystem")
	}
}

func TestHostMemoryBytesReportsInstalledMemory(t *testing.T) {
	totalRAM, err := hostMemoryBytes()
	if err != nil {
		t.Fatal(err)
	}
	if totalRAM == 0 {
		t.Error("hostMemoryBytes() = 0, want installed memory")
	}
}

// A zero peak would let the RSS regression gate pass without measuring
// anything, so the sampler's source has to report a live figure.
func TestCurrentRSSReportsProcessMemory(t *testing.T) {
	if value := currentRSS(); value <= 0 {
		t.Errorf("currentRSS() = %d, want the resident size of this process", value)
	}
}
