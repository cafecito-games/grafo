package eval_test

import (
	"context"
	"path/filepath"
	"testing"

	evaluation "github.com/cafecito-games/grafo/internal/eval"
)

func TestResolutionCorpus(t *testing.T) {
	root, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatal(err)
	}
	if err := evaluation.RunCorpus(context.Background(), root, false); err != nil {
		t.Fatal(err)
	}
}
