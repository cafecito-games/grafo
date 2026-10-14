package source

import (
	"context"
	"errors"
	"github.com/cafecito-games/grafo/internal/testtemp"
	"os"
	"path/filepath"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
)

func sessionFixture(t *testing.T, content string, budget SourceBudget) (*Session, graph.Node, string) {
	t.Helper()
	root := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(root, "code.go"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	node := graph.Node{ID: "n", Location: graph.Location{Path: "code.go", Line: 1, EndLine: 3}}
	return NewScopedService(nil, []indexer.Project{{Name: "repo", Root: root}}).NewSession(budget), node, root
}

func TestSessionAggregatesLinesAndCachesBytes(t *testing.T) {
	s, n, _ := sessionFixture(t, "one\ntwo\nthree\n", SourceBudget{MaxBytes: 14, MaxLines: 4})
	first, err := s.ReadNode(context.Background(), n, "repo", 0, 3)
	if err != nil || first.Truncated || first.Content != "one\ntwo\nthree" {
		t.Fatalf("%+v %v", first, err)
	}
	second, err := s.ReadNode(context.Background(), n, "repo", 0, 3)
	if err != nil || !second.Truncated || second.Content != "one" {
		t.Fatalf("%+v %v", second, err)
	}
	if s.Usage() != (SourceUsage{BytesRead: 14, LinesReturned: 4}) {
		t.Fatal(s.Usage())
	}
	if _, err = s.ReadNode(context.Background(), n, "repo", 0, 1); !errors.Is(err, ErrBudget) {
		t.Fatal(err)
	}
}

func TestSessionByteCapNeverReturnsPartialLine(t *testing.T) {
	s, n, root := sessionFixture(t, "one\ntwo\nthree\n", SourceBudget{MaxBytes: 6, MaxLines: 100})
	e, err := s.ReadNode(context.Background(), n, "repo", 0, 30)
	if err != nil || e.Content != "one" || !e.Truncated || s.Usage().BytesRead != 6 {
		t.Fatalf("%+v %+v %v", e, s.Usage(), err)
	}
	n.Location.Line = 2
	if _, err = s.ReadNode(context.Background(), n, "repo", 0, 30); !errors.Is(err, ErrBudget) {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "other.go"), []byte("more"), 0600); err != nil {
		t.Fatal(err)
	}
	n.Location.Path = "other.go"
	n.Location.Line = 1
	if _, err = s.ReadNode(context.Background(), n, "repo", 0, 30); !errors.Is(err, ErrBudget) {
		t.Fatal(err)
	}
	if s.Usage().BytesRead != 6 {
		t.Fatal(s.Usage())
	}
}

func TestSessionCancellationAndRepositoryIsolation(t *testing.T) {
	s, n, root := sessionFixture(t, "one\n", SourceBudget{MaxBytes: 100, MaxLines: 100})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.ReadNode(ctx, n, "repo", 0, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.ReadNode(context.Background(), n, "other", 0, 1); err == nil {
		t.Fatal("accepted other repository")
	}
	outside := filepath.Join(testtemp.Dir(t), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	n.Location.Path = "escape"
	if _, err := s.ReadNode(context.Background(), n, "repo", 0, 1); err == nil {
		t.Fatal("accepted escape")
	}
	if s.Usage() != (SourceUsage{}) {
		t.Fatal(s.Usage())
	}
}

func TestSessionScopesCollidingNodeIDs(t *testing.T) {
	s, n, _ := sessionFixture(t, "first", SourceBudget{MaxBytes: 100, MaxLines: 100})
	other := testtemp.Dir(t)
	if err := os.WriteFile(filepath.Join(other, "code.go"), []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	s.service.projects = append(s.service.projects, indexer.Project{Name: "other", Root: other})
	if _, err := s.ReadNode(context.Background(), n, "", 0, 1); err == nil {
		t.Fatal("accepted ambiguous repository")
	}
	e, err := s.ReadNode(context.Background(), n, "other", 0, 1)
	if err != nil || e.Content != "second" {
		t.Fatalf("%+v %v", e, err)
	}
}

func TestSessionRejectsProjectedBindingAsImplementationSource(t *testing.T) {
	s, n, _ := sessionFixture(t, "generator configuration", SourceBudget{MaxBytes: 100, MaxLines: 100})
	n.Properties = map[string]string{"generated_binding": "true"}
	if _, err := s.ReadNode(context.Background(), n, "repo", 0, 1); err == nil {
		t.Fatal("configuration returned as implementation")
	}
	if s.Usage() != (SourceUsage{}) {
		t.Fatal(s.Usage())
	}
}

func TestSessionSmallFileContextRespectsEveryLineCap(t *testing.T) {
	for _, tc := range []struct {
		name         string
		budget       SourceBudget
		excerptLimit int
		want         string
	}{
		{"complete", SourceBudget{MaxBytes: 100, MaxLines: 10, SmallFileContext: 4}, 10, "one\ntwo\nthree"},
		{"global cap", SourceBudget{MaxBytes: 100, MaxLines: 1, SmallFileContext: 4}, 10, "two"},
		{"excerpt cap", SourceBudget{MaxBytes: 100, MaxLines: 10, SmallFileContext: 4}, 1, "two"},
		{"partial file", SourceBudget{MaxBytes: 9, MaxLines: 10, SmallFileContext: 4}, 10, "two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, n, _ := sessionFixture(t, "one\ntwo\nthree\n", tc.budget)
			n.Location.Line = 2
			n.Location.EndLine = 2
			e, err := s.ReadNode(context.Background(), n, "repo", 0, tc.excerptLimit)
			if err != nil || e.Content != tc.want || e.SymbolStart != 2 || e.SymbolEnd != 2 {
				t.Fatalf("%+v %v", e, err)
			}
			if s.Usage().LinesReturned > tc.budget.MaxLines || s.Usage().LinesReturned > tc.excerptLimit {
				t.Fatal(s.Usage())
			}
		})
	}
}
