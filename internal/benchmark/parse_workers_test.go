package benchmark

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// TestRunRejectsAnUnusableParseWorkerCount covers the bound on the exposed
// knob. The indexer honors a positive count exactly as given, so a count this
// harness accepts is a count it will try to run; rejecting an unusable one here
// is what keeps a mistyped value from measuring something other than what was
// asked for.
func TestRunRejectsAnUnusableParseWorkerCount(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		workers int
	}{
		{name: "negative", workers: -1},
		{name: "above the ceiling", workers: 16*runtime.NumCPU() + 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Run(context.Background(), Options{ParseWorkers: testCase.workers})
			if err == nil {
				t.Fatalf("parse workers = %d was accepted", testCase.workers)
			}
			if !strings.Contains(err.Error(), "parse workers") {
				t.Fatalf("error does not name the rejected input: %v", err)
			}
		})
	}
}
