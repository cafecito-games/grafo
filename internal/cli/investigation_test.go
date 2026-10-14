package cli

import (
	"context"
	"encoding/json"
	sourcecontext "github.com/cafecito-games/grafo/internal/source"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/investigation"
)

func TestInvestigationArgumentsPreserveSharedRequest(t *testing.T) {
	args, err := parseArguments([]string{"investigate", "where is checkout", "--repo-name", "shop", "--component", "api", "--path-prefix", "src", "--path-prefix", "lib", "--language", "go,typescript", "--max-candidates", "3", "--max-source-lines", "80", "--max-source-bytes", "4096", "--max-output-bytes", "8000", "--max-work", "1000", "--timeout-ms", "900"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := investigationRequest(args)
	if err != nil {
		t.Fatal(err)
	}
	want := investigation.Request{Query: "where is checkout", Repository: "shop", Component: "api", PathPrefixes: []string{"lib", "src"}, Languages: []string{"go", "typescript"}, MaxCandidates: 3, MaxSourceLines: 80, MaxSourceBytes: 4096, MaxOutputBytes: 8000, MaxWork: 1000, TimeoutMS: 900}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v; want %#v", got, want)
	}
}

func TestInvestigationRejectsMalformedArguments(t *testing.T) {
	for _, argv := range [][]string{{"investigate"}, {"investigate", "a", "b"}, {"investigate", "a", "--max-work", "-1"}, {"investigate", "a", "--timeout-ms", "oops"}, {"investigate", "a", "--path-prefix", "../outside"}} {
		args, err := parseArguments(argv)
		if err == nil {
			_, err = investigationRequest(args)
		}
		if err == nil {
			t.Errorf("accepted %q", argv)
		}
	}
}

func TestInvestigationCLIUsesSharedBoundedReport(t *testing.T) {
	root := indexedRepository(t)
	argv := []string{"investigate", "Charge", "--repo", root, "--max-output-bytes", "8000", "--json"}
	stdout, stderr, code := output(t, argv...)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	args, err := parseArguments(argv)
	if err != nil {
		t.Fatal(err)
	}
	request, err := investigationRequest(args)
	if err != nil {
		t.Fatal(err)
	}
	repo, projects, closeRepo, err := openRead(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRepo()
	report, err := investigation.New(repo, sourcecontext.NewScopedService(repo, projects)).Investigate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSuffix(stdout, "\n") != string(expected) {
		t.Fatalf("CLI report differs from service: %s", stdout)
	}
	if len(expected) > 8000 {
		t.Fatalf("output budget exceeded: %d", len(expected))
	}
	if len(report.Excerpts) == 0 {
		t.Fatal("fixture implementation source missing")
	}
}
