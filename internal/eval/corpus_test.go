package eval_test

import (
	"context"
	"path/filepath"
	"testing"

	evaluation "github.com/cafecito-games/grafo/internal/eval"
)

// TestResolutionCorpus runs each corpus case as its own parallel subtest. A case
// copies its fixtures into a private workspace and opens its own database, so
// nothing is shared across cases, and a failure names the one case that produced
// it instead of ending the whole run.
func TestResolutionCorpus(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	cases, err := evaluation.LoadCorpus(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range cases {
		t.Run(item.Manifest.CaseID, func(t *testing.T) {
			t.Parallel()
			if err := evaluation.VerifyCase(context.Background(), item); err != nil {
				t.Fatal(err)
			}
		})
	}
}
