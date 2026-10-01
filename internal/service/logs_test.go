package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestLoggerRedactsControlCharactersAndRecordsOnlyMetadata(t *testing.T) {
	path := filepath.Join(testtemp.Dir(t), "logs", "service.log")
	logger, err := OpenLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()
	logger.Record("info", "indexed root", map[string]string{
		"root": "/repos/app", "updated": "3", "ms": "42",
		"error": "parse failed\nsecret-token=abc\n",
	})
	lines, err := TailLog(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected one record, got %#v", lines)
	}
	line := lines[0]
	for _, want := range []string{"indexed root", "root=/repos/app", "updated=3", "ms=42"} {
		if !strings.Contains(line, want) {
			t.Fatalf("record %q is missing %q", line, want)
		}
	}
	if strings.Count(line, "\n") != 0 {
		t.Fatalf("a multi-line field forged extra records: %q", line)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("log mode = %v, want 0600", mode)
	}
}

func TestLoggerRotatesWithinItsBound(t *testing.T) {
	path := filepath.Join(testtemp.Dir(t), "service.log")
	logger, err := OpenLogger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()
	filler := strings.Repeat("p", 4096)
	for index := 0; index < 600; index++ {
		logger.Record("info", "indexed root", map[string]string{"root": filler})
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > MaxLogBytes {
		t.Fatalf("log grew to %d bytes, above the %d bound", info.Size(), MaxLogBytes)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected one rotated log: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err == nil {
		t.Fatal("rotation kept more history than LogHistory allows")
	}
}

func TestTailLogTreatsAMissingLogAsEmpty(t *testing.T) {
	lines, err := TailLog(filepath.Join(testtemp.Dir(t), "absent.log"), 10)
	if err != nil {
		t.Fatalf("TailLog: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("expected no lines, got %#v", lines)
	}
}
