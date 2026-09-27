package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/federation"
	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	parserdefaults "github.com/cafecito-games/grafo/internal/parser/defaults"
	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

type Snapshot struct {
	Nodes   []NodeRef   `json:"nodes"`
	Edges   []EdgeRef   `json:"edges"`
	Queries []QuerySpec `json:"queries,omitempty"`
}

type caseResult struct {
	loaded   LoadedManifest
	snapshot Snapshot
}

// RunCorpus validates every manifest before indexing, then runs every case
// through initial, unchanged-incremental, and fresh-database evaluation.
func RunCorpus(ctx context.Context, root string, update bool) error {
	loaded, err := loadCorpus(root)
	if err != nil {
		return err
	}
	results := make([]caseResult, 0, len(loaded))
	for _, item := range loaded {
		result, err := runCase(ctx, item, update)
		if err != nil {
			return fmt.Errorf("case %s: %w", item.Manifest.CaseID, err)
		}
		results = append(results, caseResult{loaded: item, snapshot: result})
	}
	if update {
		for _, result := range results {
			manifest := result.loaded.Manifest
			manifest.Expect.Nodes = result.snapshot.Nodes
			manifest.Expect.Edges = result.snapshot.Edges
			manifest.Expect.Queries = result.snapshot.Queries
			if err := writeManifestAtomic(result.loaded.Path, manifest); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadCorpus(root string) ([]LoadedManifest, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read corpus %s: %w", root, err)
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, fmt.Errorf("unexpected corpus entry %s", filepath.Join(root, entry.Name()))
		}
		path := filepath.Join(root, entry.Name(), "manifest.json")
		if _, err := os.Stat(path); err == nil {
			paths = append(paths, path)
		} else if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("case directory %s has no manifest.json", filepath.Join(root, entry.Name()))
		} else {
			return nil, err
		}
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, fmt.Errorf("corpus %s contains no case manifests", root)
	}
	loaded := make([]LoadedManifest, 0, len(paths))
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		manifest, loadErr := LoadManifest(path, file)
		closeErr := file.Close()
		if loadErr != nil {
			return nil, loadErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		loaded = append(loaded, LoadedManifest{Path: path, Manifest: manifest})
	}
	if err := ValidateManifests(loaded); err != nil {
		return nil, err
	}
	return loaded, nil
}

func runCase(ctx context.Context, loaded LoadedManifest, update bool) (Snapshot, error) {
	caseDir := filepath.Dir(loaded.Path)
	if err := validateFixtureRepositories(caseDir, loaded.Manifest.Repositories); err != nil {
		return Snapshot{}, err
	}
	first, incremental, err := runWorkspace(ctx, caseDir, loaded.Manifest)
	if err != nil {
		return Snapshot{}, err
	}
	if err := compareSnapshots("unchanged incremental run", first, incremental); err != nil {
		return Snapshot{}, err
	}
	fresh, freshIncremental, err := runWorkspace(ctx, caseDir, loaded.Manifest)
	if err != nil {
		return Snapshot{}, fmt.Errorf("fresh database run: %w", err)
	}
	if err := compareSnapshots("fresh database run", first, fresh); err != nil {
		return Snapshot{}, err
	}
	if err := compareSnapshots("fresh unchanged incremental run", fresh, freshIncremental); err != nil {
		return Snapshot{}, err
	}
	if !update {
		expected := Snapshot{Nodes: loaded.Manifest.Expect.Nodes, Edges: loaded.Manifest.Expect.Edges, Queries: loaded.Manifest.Expect.Queries}
		if err := compareSnapshots("committed manifest", expected, first); err != nil {
			return Snapshot{}, err
		}
	}
	return first, nil
}

func runWorkspace(ctx context.Context, caseDir string, manifest Manifest) (Snapshot, Snapshot, error) {
	workspace, err := os.MkdirTemp("", "grafo-eval-"+manifest.CaseID+"-")
	if err != nil {
		return Snapshot{}, Snapshot{}, err
	}
	defer os.RemoveAll(workspace)

	projects := make([]indexer.Project, 0, len(manifest.Repositories))
	roots := make([]string, 0, len(manifest.Repositories))
	origins := map[string]map[string]bool{}
	var diagnostics []DiagnosticRef
	for _, spec := range manifest.Repositories {
		source := filepath.Join(caseDir, filepath.FromSlash(spec.Path))
		destination := filepath.Join(workspace, spec.ID)
		if err := copyTree(source, destination); err != nil {
			return Snapshot{}, Snapshot{}, fmt.Errorf("copy repository %s: %w", spec.ID, err)
		}
		if err := initializeFixtureGit(ctx, destination, manifest.CaseID, spec.ID); err != nil {
			return Snapshot{}, Snapshot{}, fmt.Errorf("initialize repository %s: %w", spec.ID, err)
		}
		project, err := indexer.DiscoverProject(ctx, destination)
		if err != nil {
			return Snapshot{}, Snapshot{}, err
		}
		projects = append(projects, project)
		roots = append(roots, destination)
		report, err := indexOnce(ctx, project, false)
		if err != nil {
			return Snapshot{}, Snapshot{}, fmt.Errorf("initial index %s: %w", spec.ID, err)
		}
		for _, diagnostic := range report.Diagnostics {
			diagnostics = append(diagnostics, DiagnosticRef{Repo: spec.ID, Path: diagnostic.Path,
				Line: diagnostic.Line, Level: diagnostic.Level, Message: diagnostic.Message})
		}
		if err := collectOrigins(ctx, project, spec.ID, origins); err != nil {
			return Snapshot{}, Snapshot{}, err
		}
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		return left.Repo+"\x00"+left.Path+fmt.Sprintf("\x00%09d", left.Line)+"\x00"+left.Level+"\x00"+left.Message <
			right.Repo+"\x00"+right.Path+fmt.Sprintf("\x00%09d", right.Line)+"\x00"+right.Level+"\x00"+right.Message
	})
	if !equalSlices(manifest.Expect.Diagnostics, diagnostics) {
		return Snapshot{}, Snapshot{}, focusedSliceDiff("diagnostics", manifest.Expect.Diagnostics, diagnostics)
	}
	first, err := evaluateWorkspace(ctx, projects, roots, origins, manifest.Expect)
	if err != nil {
		return Snapshot{}, Snapshot{}, err
	}
	for index, project := range projects {
		if _, err := indexOnce(ctx, project, true); err != nil {
			return Snapshot{}, Snapshot{}, fmt.Errorf("incremental index %s: %w", manifest.Repositories[index].ID, err)
		}
	}
	incremental, err := evaluateWorkspace(ctx, projects, roots, origins, manifest.Expect)
	return first, incremental, err
}

func initializeFixtureGit(ctx context.Context, root, caseID, repositoryID string) error {
	commands := [][]string{
		{"init", "-b", "eval"},
		{"remote", "add", "origin", "https://fixtures.invalid/" + caseID + "/" + repositoryID + ".git"},
	}
	for _, arguments := range commands {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func indexOnce(ctx context.Context, project indexer.Project, requireUnchanged bool) (indexer.Report, error) {
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		return indexer.Report{}, err
	}
	report, runErr := indexer.NewService(repository, parserdefaults.NewRegistry()).Run(ctx, project, indexer.Options{})
	closeErr := repository.Close()
	if runErr != nil {
		return report, runErr
	}
	if closeErr != nil {
		return report, closeErr
	}
	if requireUnchanged && (len(report.Updated) != 0 || len(report.Removed) != 0 || report.Unchanged != report.Counts.Files) {
		return report, fmt.Errorf("unchanged refresh mutated index: updated=%v removed=%v unchanged=%d files=%d", report.Updated, report.Removed, report.Unchanged, report.Counts.Files)
	}
	return report, nil
}

func collectOrigins(ctx context.Context, project indexer.Project, repoID string, origins map[string]map[string]bool) error {
	repository, err := sqlite.Open(ctx, project.IndexPath)
	if err != nil {
		return err
	}
	defer repository.Close()
	nodes, err := repository.SearchNodes(ctx, "", 1_000_000)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if origins[node.ID] == nil {
			origins[node.ID] = map[string]bool{}
		}
		origins[node.ID][repoID] = true
	}
	return nil
}

func evaluateWorkspace(ctx context.Context, projects []indexer.Project, roots []string, origins map[string]map[string]bool, expected Expectations) (Snapshot, error) {
	repository, closeRepository, err := openReadRepository(ctx, projects, roots)
	if err != nil {
		return Snapshot{}, err
	}
	defer closeRepository()
	snapshot, _, err := captureSnapshot(ctx, repository, origins)
	if err != nil {
		return Snapshot{}, err
	}
	queries, err := evaluateQueries(ctx, repository, origins, expected.Queries)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot.Queries = queries
	if err := evaluateAmbiguities(ctx, repository, origins, expected.Ambiguities); err != nil {
		return Snapshot{}, err
	}
	if err := evaluateForbiddenEdges(expected.ForbiddenEdges, snapshot.Nodes, snapshot.Edges); err != nil {
		return Snapshot{}, err
	}
	if err := evaluateForbiddenPaths(ctx, repository, expected.ForbiddenPaths); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func openReadRepository(ctx context.Context, projects []indexer.Project, roots []string) (graph.ReadRepository, func() error, error) {
	if len(projects) == 1 {
		repository, err := sqlite.Open(ctx, projects[0].IndexPath)
		if err != nil {
			return nil, nil, err
		}
		return repository, repository.Close, nil
	}
	repository, err := federation.Open(ctx, roots)
	if err != nil {
		return nil, nil, err
	}
	return repository, repository.Close, nil
}

func captureSnapshot(ctx context.Context, repository graph.QueryRepository, origins map[string]map[string]bool) (Snapshot, map[string]graph.Node, error) {
	nodes, err := repository.SearchNodes(ctx, "", 1_000_000)
	if err != nil {
		return Snapshot{}, nil, err
	}
	nodesByID := make(map[string]graph.Node, len(nodes))
	refsByID := make(map[string]NodeRef, len(nodes))
	result := Snapshot{Nodes: make([]NodeRef, 0, len(nodes)), Edges: []EdgeRef{}}
	for _, node := range nodes {
		nodesByID[node.ID] = node
		ref := canonicalNode(node, origins)
		refsByID[node.ID] = ref
		result.Nodes = append(result.Nodes, ref)
	}
	sortNodeRefs(result.Nodes)
	seenEdges := map[string]bool{}
	for _, node := range nodes {
		edges, err := repository.EdgesFrom(ctx, node.ID)
		if err != nil {
			return Snapshot{}, nil, err
		}
		for _, edge := range edges {
			if seenEdges[edge.ID] {
				continue
			}
			to, ok := refsByID[edge.ToID]
			if !ok {
				target, err := repository.Node(ctx, edge.ToID)
				if err != nil {
					return Snapshot{}, nil, fmt.Errorf("edge %s target %s: %w", edge.ID, edge.ToID, err)
				}
				to = canonicalNode(target, origins)
				refsByID[edge.ToID] = to
				nodesByID[edge.ToID] = target
			}
			result.Edges = append(result.Edges, EdgeRef{
				From: refsByID[edge.FromID], Relation: edge.Kind, To: to,
				Source:     SourceRef{Path: edge.Location.Path, Line: edge.Location.Line, Column: edge.Location.Column},
				Properties: edge.Properties,
			})
			seenEdges[edge.ID] = true
		}
	}
	sortEdgeRefs(result.Edges)
	return result, nodesByID, nil
}

func canonicalNode(node graph.Node, origins map[string]map[string]bool) NodeRef {
	ref := NodeRef{Kind: node.Kind, QualifiedName: node.QualifiedName, External: node.External}
	if repositories := origins[node.ID]; len(repositories) == 1 {
		for repository := range repositories {
			ref.Repo = repository
		}
	}
	return ref
}

func evaluateQueries(ctx context.Context, repository graph.QueryRepository, origins map[string]map[string]bool, specifications []QuerySpec) ([]QuerySpec, error) {
	service := query.NewService(repository)
	result := make([]QuerySpec, 0, len(specifications))
	for _, specification := range specifications {
		path, err := service.ShortestPath(ctx, specification.From, specification.To, query.Direction(specification.Direction), specification.Relations, 10_000)
		if err != nil {
			return nil, fmt.Errorf("query %q (%s -> %s): %w", specification.ID, specification.From, specification.To, err)
		}
		actual := specification
		actual.Nodes = make([]NodeRef, 0, len(path.Nodes))
		for _, node := range path.Nodes {
			actual.Nodes = append(actual.Nodes, canonicalNode(node, origins))
		}
		actual.Edges = make([]EdgeRef, 0, len(path.Edges))
		for _, edge := range path.Edges {
			from, err := repository.Node(ctx, edge.FromID)
			if err != nil {
				return nil, err
			}
			to, err := repository.Node(ctx, edge.ToID)
			if err != nil {
				return nil, err
			}
			actual.Edges = append(actual.Edges, EdgeRef{
				From: canonicalNode(from, origins), Relation: edge.Kind, To: canonicalNode(to, origins),
				Source:     SourceRef{Path: edge.Location.Path, Line: edge.Location.Line, Column: edge.Location.Column},
				Properties: edge.Properties,
			})
		}
		result = append(result, actual)
	}
	return result, nil
}

func evaluateAmbiguities(ctx context.Context, repository graph.QueryRepository, origins map[string]map[string]bool, specifications []AmbiguitySpec) error {
	service := query.NewService(repository)
	for _, specification := range specifications {
		_, err := service.Resolve(ctx, specification.Selector)
		var ambiguous *query.AmbiguousError
		if !errors.As(err, &ambiguous) {
			return fmt.Errorf("selector %q: expected ambiguity, got %v", specification.Selector, err)
		}
		actual := make([]NodeRef, 0, len(ambiguous.Candidates))
		for _, node := range ambiguous.Candidates {
			actual = append(actual, canonicalNode(node, origins))
		}
		sortNodeRefs(actual)
		expected := append([]NodeRef(nil), specification.Candidates...)
		sortNodeRefs(expected)
		if !reflect.DeepEqual(expected, actual) {
			return fmt.Errorf("selector %q ambiguity candidates differ:\nwant: %+v\n got: %+v", specification.Selector, expected, actual)
		}
	}
	return nil
}

func evaluateForbiddenEdges(patterns []EdgePattern, nodes []NodeRef, edges []EdgeRef) error {
	for _, pattern := range patterns {
		if countNodeMatches(nodes, pattern.From) != 1 {
			return fmt.Errorf("forbidden edge source selector must resolve exactly once: %s", formatNode(pattern.From))
		}
		if countNodeMatches(nodes, pattern.To) != 1 {
			return fmt.Errorf("forbidden edge target selector must resolve exactly once: %s", formatNode(pattern.To))
		}
		for _, edge := range edges {
			if nodeRefMatches(pattern.From, edge.From) && pattern.Relation == edge.Relation && nodeRefMatches(pattern.To, edge.To) {
				return fmt.Errorf("forbidden edge exists: %s %s %s at %s:%d", formatNode(pattern.From), pattern.Relation, formatNode(pattern.To), edge.Source.Path, edge.Source.Line)
			}
		}
	}
	return nil
}

func countNodeMatches(nodes []NodeRef, ref NodeRef) int {
	count := 0
	for _, node := range nodes {
		if nodeRefMatches(ref, node) {
			count++
		}
	}
	return count
}

func evaluateForbiddenPaths(ctx context.Context, repository graph.QueryRepository, patterns []PathPattern) error {
	service := query.NewService(repository)
	for _, pattern := range patterns {
		path, err := service.ShortestPath(ctx, pattern.From, pattern.To, query.Direction(pattern.Direction), pattern.Relations, 10_000)
		if err == nil {
			return fmt.Errorf("forbidden path exists from %s to %s: %v", pattern.From, pattern.To, qualifiedNames(path.Nodes))
		}
		if !strings.HasPrefix(err.Error(), "no path from ") {
			return fmt.Errorf("forbidden path %s -> %s did not resolve both endpoints: %w", pattern.From, pattern.To, err)
		}
	}
	return nil
}

func qualifiedNames(nodes []graph.Node) []string {
	result := make([]string, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, node.QualifiedName)
	}
	return result
}

func nodeRefMatches(pattern, actual NodeRef) bool {
	return (pattern.Repo == "" || pattern.Repo == actual.Repo) && pattern.Kind == actual.Kind &&
		pattern.QualifiedName == actual.QualifiedName && pattern.External == actual.External
}

func compareSnapshots(label string, expected, actual Snapshot) error {
	if !equalSlices(expected.Nodes, actual.Nodes) {
		return focusedSliceDiff(label+" nodes", expected.Nodes, actual.Nodes)
	}
	if !equalSlices(expected.Edges, actual.Edges) {
		return focusedSliceDiff(label+" edges", expected.Edges, actual.Edges)
	}
	if !equalSlices(expected.Queries, actual.Queries) {
		return focusedSliceDiff(label+" queries", expected.Queries, actual.Queries)
	}
	return nil
}

func equalSlices(expected, actual any) bool {
	expectedValue, actualValue := reflect.ValueOf(expected), reflect.ValueOf(actual)
	if expectedValue.Len() != actualValue.Len() {
		return false
	}
	for index := range expectedValue.Len() {
		if !reflect.DeepEqual(expectedValue.Index(index).Interface(), actualValue.Index(index).Interface()) {
			return false
		}
	}
	return true
}

func focusedSliceDiff(label string, expected, actual any) error {
	expectedValue, actualValue := reflect.ValueOf(expected), reflect.ValueOf(actual)
	common := min(expectedValue.Len(), actualValue.Len())
	for index := range common {
		if reflect.DeepEqual(expectedValue.Index(index).Interface(), actualValue.Index(index).Interface()) {
			continue
		}
		expectedJSON, _ := json.MarshalIndent(expectedValue.Index(index).Interface(), "", "  ")
		actualJSON, _ := json.MarshalIndent(actualValue.Index(index).Interface(), "", "  ")
		return fmt.Errorf("%s differ at index %d (expected %d, actual %d)\n--- expected\n%s\n+++ actual\n%s", label, index, expectedValue.Len(), actualValue.Len(), expectedJSON, actualJSON)
	}
	if expectedValue.Len() > common {
		expectedJSON, _ := json.MarshalIndent(expectedValue.Index(common).Interface(), "", "  ")
		return fmt.Errorf("%s missing item at index %d (expected %d, actual %d):\n%s", label, common, expectedValue.Len(), actualValue.Len(), expectedJSON)
	}
	if actualValue.Len() == common {
		return fmt.Errorf("%s differ only in representation (expected %d, actual %d)", label, expectedValue.Len(), actualValue.Len())
	}
	actualJSON, _ := json.MarshalIndent(actualValue.Index(common).Interface(), "", "  ")
	return fmt.Errorf("%s has unexpected item at index %d (expected %d, actual %d):\n%s", label, common, expectedValue.Len(), actualValue.Len(), actualJSON)
}

func sortEdgeRefs(edges []EdgeRef) {
	sort.Slice(edges, func(i, j int) bool {
		left, right := edgeSortKey(edges[i]), edgeSortKey(edges[j])
		return left < right
	})
}

func edgeSortKey(edge EdgeRef) string {
	properties, _ := json.Marshal(edge.Properties)
	return formatNode(edge.From) + "\x00" + string(edge.Relation) + "\x00" + formatNode(edge.To) + "\x00" +
		edge.Source.Path + fmt.Sprintf("\x00%09d\x00%09d\x00%s", edge.Source.Line, edge.Source.Column, properties)
}

func formatNode(node NodeRef) string {
	return node.Repo + ":" + string(node.Kind) + ":" + node.QualifiedName + fmt.Sprintf(":external=%t", node.External)
}

func validateFixtureRepositories(caseDir string, specifications []RepositorySpec) error {
	wanted := map[string]bool{}
	for _, specification := range specifications {
		clean := filepath.ToSlash(filepath.Clean(specification.Path))
		if !strings.HasPrefix(clean, "repos/") || strings.TrimPrefix(clean, "repos/") == "" || strings.Contains(strings.TrimPrefix(clean, "repos/"), "/") {
			return fmt.Errorf("repository %q path must be one direct child of repos/", specification.ID)
		}
		wanted[strings.TrimPrefix(clean, "repos/")] = true
	}
	entries, err := os.ReadDir(filepath.Join(caseDir, "repos"))
	if err != nil {
		return fmt.Errorf("read fixture repositories: %w", err)
	}
	actual := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			return fmt.Errorf("unexpected non-repository fixture %s", filepath.Join("repos", entry.Name()))
		}
		actual[entry.Name()] = true
	}
	if !reflect.DeepEqual(wanted, actual) {
		return fmt.Errorf("fixture repository set differs: manifest=%v disk=%v", sortedKeys(wanted), sortedKeys(actual))
	}
	return nil
}

func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func copyTree(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture symlink is not allowed: %s", path)
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			input.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		return errors.Join(copyErr, input.Close(), output.Close())
	})
}

func writeManifestAtomic(path string, manifest Manifest) error {
	content, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".manifest-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace manifest %s: %w", path, err)
	}
	return nil
}
