package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
)

// TestPrintIndexReportSeparatesSuppressedAndUnavailableCounts keeps a failed
// count query from being reported as a summary the caller never asked for.
func TestPrintIndexReportSeparatesSuppressedAndUnavailableCounts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name            string
		report          indexer.Report
		countsRequested bool
		want            string
		reject          []string
	}{
		{
			name:   "collected",
			report: indexer.Report{CountsCollected: true, Counts: graph.Counts{Files: 2, Nodes: 6, Edges: 5, External: 1}},
			want:   "2 files · 6 nodes · 5 edges · 1 unresolved",
		},
		{
			name:   "suppressed",
			report: indexer.Report{ElapsedMS: 12},
			want:   "counts not collected (add --counts)",
			reject: []string{"0 files", "0 nodes"},
		},
		{
			name:            "requested but unavailable",
			report:          indexer.Report{ElapsedMS: 12},
			countsRequested: true,
			want:            "counts unavailable (see diagnostics)",
			reject:          []string{"add --counts", "0 files", "0 nodes"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			application := New(&stdout, &stderr)
			if err := application.printIndexReport(test.report, false, test.countsRequested); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("report = %q, want %q", stdout.String(), test.want)
			}
			for _, rejected := range test.reject {
				if strings.Contains(stdout.String(), rejected) {
					t.Fatalf("report = %q, must not contain %q", stdout.String(), rejected)
				}
			}
		})
	}
}
