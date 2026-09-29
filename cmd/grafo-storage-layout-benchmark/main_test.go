package main

import (
	"strings"
	"testing"
)

// TestEnvironmentIntFailsNamingTheVariable proves a malformed numeric
// environment value is reported with the offending variable's name instead of
// silently degrading into an invalid default.
func TestEnvironmentIntFailsNamingTheVariable(t *testing.T) {
	t.Setenv("GRAFO_STORAGE_LAYOUT_SAMPLES", "three")
	_, err := environmentInt("GRAFO_STORAGE_LAYOUT_SAMPLES", 1)
	if err == nil || !strings.Contains(err.Error(), "GRAFO_STORAGE_LAYOUT_SAMPLES") {
		t.Fatalf("malformed value accepted: %v", err)
	}

	t.Setenv("GRAFO_STORAGE_LAYOUT_REPETITIONS", "11")
	parsed, err := environmentInt("GRAFO_STORAGE_LAYOUT_REPETITIONS", 7)
	if err != nil || parsed != 11 {
		t.Fatalf("valid value not parsed: %d %v", parsed, err)
	}
}
