package layoutbench

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/semantic"
)

// EquivalenceRepository is the full adapter surface the driver exercises. A
// production *sqlite.Repository and a *VariantRepository both satisfy it.
type EquivalenceRepository interface {
	graph.Repository
	graph.CatalogRepository
	graph.RelationEdgeRepository
	graph.ExternalEdgeRepository
	graph.ExternalNodeRepository
	graph.InstrumentedIndexRepository
	graph.InstrumentedWriteRepository
	graph.ReconciliationStatusRepository
	semantic.Repository
}

// RepositoryOpener opens one adapter instance at path.
type RepositoryOpener func(ctx context.Context, path string) (EquivalenceRepository, error)

// EquivalenceInput wires the two adapters under comparison and the corpora the
// scenarios run. The main fixture should be probe-suite scale; the batch
// interruption fixture must exceed one reconciliation batch of dirty facts.
type EquivalenceInput struct {
	Control                  RepositoryOpener
	Variant                  RepositoryOpener
	Fixture                  Fixture
	BatchInterruptionFixture Fixture
	ControlDirectory         string
	VariantDirectory         string
}

// EquivalenceResult reports the comparison. Equivalent is true only when every
// probe of every scenario rendered byte-identical output on both adapters;
// FirstDifference names the first probe that disagreed.
type EquivalenceResult struct {
	Equivalent      bool
	FirstDifference string
	ControlDigest   string
	VariantDigest   string
	ControlProbes   int
	VariantProbes   int
}

const (
	equivalenceEmbeddingModel = "equivalence-model"
	equivalenceRoot           = "/equivalence/fixture"
)

// errEquivalenceInterrupt stops reconciliation after one committed batch; it
// simulates a process interruption at the reconciliation_batch boundary.
var errEquivalenceInterrupt = errors.New("layoutbench: interrupt at reconciliation boundary")

// RunEquivalence drives both adapters through the same scripted scenarios and
// compares every probe output. The scenarios cover the full access-pattern
// surface, ambiguity and folding behavior, later-declaration and orphan
// cleanup convergence, incremental edit/delete/restore/branch-switch,
// cancellation mid file replacement, and an interruption at every indexer
// boundary kind followed by reopen and resume.
func RunEquivalence(ctx context.Context, input EquivalenceInput) (EquivalenceResult, error) {
	if input.Control == nil || input.Variant == nil {
		return EquivalenceResult{}, errors.New("equivalence input needs both openers")
	}
	if len(input.Fixture.Files) == 0 || len(input.BatchInterruptionFixture.Files) == 0 {
		return EquivalenceResult{}, errors.New("equivalence input needs both fixtures")
	}
	if input.ControlDirectory == "" || input.VariantDirectory == "" {
		return EquivalenceResult{}, errors.New("equivalence input needs both directories")
	}
	controlLog := &sessionLog{adapter: "control"}
	variantLog := &sessionLog{adapter: "variant"}

	baselineDigest, err := runMainScenario(ctx, input.Control, input.ControlDirectory, input.Fixture, controlLog)
	if err != nil {
		return EquivalenceResult{}, fmt.Errorf("control main scenario: %w", err)
	}
	if err := runIncrementalScenario(ctx, input.Control, input.ControlDirectory, input.Fixture, controlLog); err != nil {
		return EquivalenceResult{}, fmt.Errorf("control incremental scenario: %w", err)
	}
	if err := runCancellationScenario(ctx, input.Control, input.ControlDirectory, input.Fixture, controlLog); err != nil {
		return EquivalenceResult{}, fmt.Errorf("control cancellation scenario: %w", err)
	}
	if err := runBoundaryScenarios(ctx, input.Control, input.ControlDirectory,
		input.Fixture, input.BatchInterruptionFixture, baselineDigest, controlLog); err != nil {
		return EquivalenceResult{}, fmt.Errorf("control boundary scenarios: %w", err)
	}

	variantBaseline, err := runMainScenario(ctx, input.Variant, input.VariantDirectory, input.Fixture, variantLog)
	if err != nil {
		return EquivalenceResult{}, fmt.Errorf("variant main scenario: %w", err)
	}
	if err := runIncrementalScenario(ctx, input.Variant, input.VariantDirectory, input.Fixture, variantLog); err != nil {
		return EquivalenceResult{}, fmt.Errorf("variant incremental scenario: %w", err)
	}
	if err := runCancellationScenario(ctx, input.Variant, input.VariantDirectory, input.Fixture, variantLog); err != nil {
		return EquivalenceResult{}, fmt.Errorf("variant cancellation scenario: %w", err)
	}
	if err := runBoundaryScenarios(ctx, input.Variant, input.VariantDirectory,
		input.Fixture, input.BatchInterruptionFixture, variantBaseline, variantLog); err != nil {
		return EquivalenceResult{}, fmt.Errorf("variant boundary scenarios: %w", err)
	}

	result := EquivalenceResult{
		ControlProbes: len(controlLog.lines),
		VariantProbes: len(variantLog.lines),
		ControlDigest: controlLog.digest(),
		VariantDigest: variantLog.digest(),
	}
	if err := assertSelfChecks(controlLog); err != nil {
		return EquivalenceResult{}, fmt.Errorf("control self-checks: %w", err)
	}
	if err := assertSelfChecks(variantLog); err != nil {
		return EquivalenceResult{}, fmt.Errorf("variant self-checks: %w", err)
	}
	result.Equivalent, result.FirstDifference = compareLogs(controlLog, variantLog)
	return result, nil
}

// selfCheckProbes are the boolean invariants each adapter run must satisfy on
// its own, independent of the control/variant comparison: scenario convergence,
// cancellation atomicity, and resume equivalence. A missing or false entry
// means the run itself is broken, not merely different from the other adapter.
var selfCheckProbes = map[string]bool{
	"interrupted":               true,
	"atomic":                    true,
	"converged":                 true,
	"resume-converged":          true,
	"matches-main-baseline":     true,
	"restored-converged":        true,
	"branch-restored-converged": true,
	"replace-owner-unchanged":   true,
}

// assertSelfChecks fails when any self-check probe is missing or did not hold.
func assertSelfChecks(log *sessionLog) error {
	seen := make(map[string]int)
	for _, line := range log.lines {
		name, value, found := strings.Cut(line, ": ")
		if !found {
			continue
		}
		scenario, probe, found := strings.Cut(name, "/")
		if !found || !selfCheckProbes[probe] {
			continue
		}
		seen[scenario+"/"+probe]++
		if value != "true" {
			return fmt.Errorf("self-check %s is %s", name, value)
		}
	}
	for probe := range selfCheckProbes {
		found := false
		for name := range seen {
			if _, nameProbe, cut := strings.Cut(name, "/"); cut && nameProbe == probe {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("self-check probe %s was never recorded", probe)
		}
	}
	return nil
}

// sessionLog collects one adapter's rendered probe outputs, in scenario order.
type sessionLog struct {
	adapter string
	lines   []string
}

func (log *sessionLog) record(scenario, probe, value string) {
	log.lines = append(log.lines, scenario+"/"+probe+": "+value)
}

func (log *sessionLog) digest() string {
	hasher := sha256.New()
	hasher.Write([]byte(strings.Join(log.lines, "\n")))
	return hex.EncodeToString(hasher.Sum(nil))
}

func compareLogs(control, variant *sessionLog) (bool, string) {
	bound := min(len(control.lines), len(variant.lines))
	for index := range bound {
		if control.lines[index] != variant.lines[index] {
			return false, fmt.Sprintf("%s: control %q, variant %q",
				control.lines[index], control.lines[index], variant.lines[index])
		}
	}
	if len(control.lines) != len(variant.lines) {
		longer := control
		if len(variant.lines) > len(control.lines) {
			longer = variant
		}
		return false, fmt.Sprintf("probe counts differ (%d control, %d variant); first extra probe %q",
			len(control.lines), len(variant.lines), longer.lines[bound])
	}
	return true, ""
}

// equivalenceSession pairs one open repository with the log that renders its
// probes. Every recorded value must be a deterministic function of the corpus.
type equivalenceSession struct {
	scenario   string
	log        *sessionLog
	repository EquivalenceRepository
}

func (session *equivalenceSession) record(probe, value string) {
	session.log.record(session.scenario, probe, value)
}

func renderError(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, sql.ErrNoRows):
		return "not-found"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	default:
		return "error: " + err.Error()
	}
}

func renderProperties(properties map[string]string) string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+properties[key])
	}
	return strings.Join(parts, ",")
}

func renderLocation(location graph.Location) string {
	return fmt.Sprintf("%s:%d:%d:%d", location.Path, location.Line, location.Column, location.EndLine)
}

func renderNode(node graph.Node) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%s|external=%t",
		node.ID, node.Kind, node.Name, node.QualifiedName, node.Language,
		renderLocation(node.Location), node.OwnerFile, renderProperties(node.Properties), node.External)
}

func renderNodes(nodes []graph.Node) string {
	rendered := make([]string, 0, len(nodes))
	for _, node := range nodes {
		rendered = append(rendered, renderNode(node))
	}
	return strings.Join(rendered, ";")
}

func renderEdge(edge graph.Edge) string {
	return fmt.Sprintf("%s|%s|%s->%s|%s|%s|%s|%s",
		edge.ID, edge.FactID, edge.FromID, edge.ToID, edge.Kind, edge.Producer,
		renderLocation(edge.Location), renderProperties(edge.Properties))
}

func renderEdges(edges []graph.Edge) string {
	rendered := make([]string, 0, len(edges))
	for _, edge := range edges {
		rendered = append(rendered, renderEdge(edge))
	}
	return strings.Join(rendered, ";")
}

func renderCounts(counts graph.Counts) string {
	kinds := make([]string, 0, len(counts.ByKind))
	for kind := range counts.ByKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	kindParts := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		kindParts = append(kindParts, fmt.Sprintf("%s=%d", kind, counts.ByKind[kind]))
	}
	edgeKinds := make([]string, 0, len(counts.ByEdge))
	for kind := range counts.ByEdge {
		edgeKinds = append(edgeKinds, kind)
	}
	sort.Strings(edgeKinds)
	edgeParts := make([]string, 0, len(edgeKinds))
	for _, kind := range edgeKinds {
		edgeParts = append(edgeParts, fmt.Sprintf("%s=%d", kind, counts.ByEdge[kind]))
	}
	return fmt.Sprintf("files=%d nodes=%d facts=%d edges=%d external=%d kinds[%s] edges-by-kind[%s]",
		counts.Files, counts.Nodes, counts.Facts, counts.Edges, counts.External,
		strings.Join(kindParts, ","), strings.Join(edgeParts, ","))
}

func renderFiles(files map[string]graph.FileRecord) string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	rendered := make([]string, 0, len(paths))
	for _, path := range paths {
		record := files[path]
		rendered = append(rendered, fmt.Sprintf("%s|%s|%s|%d|%d|%s",
			record.Path, record.Hash, record.Language, record.Size, record.ModifiedNS, record.IndexedAt))
	}
	return strings.Join(rendered, ";")
}

func renderMatchGroup(group graph.NodeMatchGroup) string {
	return fmt.Sprintf("level=%s external=%t total=%d strict=%d nodes=%s",
		group.Level, group.External, group.Total, group.Strict, renderNodes(group.Nodes))
}

func renderRelationPage(page graph.RelationEdgePage) string {
	items := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, renderEdge(item.Edge)+" => "+renderNode(item.Counterpart))
	}
	return fmt.Sprintf("truncated=%t items[%s]", page.Truncated, strings.Join(items, ";"))
}

func renderEmbeddings(embeddings []semantic.Embedding) string {
	sorted := make([]semantic.Embedding, len(embeddings))
	copy(sorted, embeddings)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].NodeID < sorted[j].NodeID })
	rendered := make([]string, 0, len(sorted))
	for _, embedding := range sorted {
		rendered = append(rendered, fmt.Sprintf("%s|%s|%s|%d|%v|%s", embedding.NodeID, embedding.Model,
			embedding.ContentHash, len(embedding.Vector), embedding.Vector, embedding.UpdatedAt))
	}
	return strings.Join(rendered, ";")
}

func renderHashes(hashes map[string]string) string {
	ids := make([]string, 0, len(hashes))
	for id := range hashes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rendered := make([]string, 0, len(ids))
	for _, id := range ids {
		rendered = append(rendered, id+"="+hashes[id])
	}
	return strings.Join(rendered, ";")
}

func renderWriteStats(stats graph.WriteStats) string {
	return fmt.Sprintf("nodes=%+v facts=%+v edges=%+v", stats.Nodes, stats.Facts, stats.Edges)
}

// digestLines hashes an ordered set of rendered lines.
func digestLines(lines []string) string {
	hasher := sha256.New()
	for _, line := range lines {
		hasher.Write([]byte(line))
		hasher.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// enumerateNodes lists every node of the graph in a deterministic order: kinds
// sorted, and ListNodesByKind orders rows within a kind.
func (session *equivalenceSession) enumerateNodes(ctx context.Context) ([]graph.Node, error) {
	counts, err := session.repository.Counts(ctx)
	if err != nil {
		return nil, err
	}
	kinds := make([]string, 0, len(counts.ByKind))
	for kind := range counts.ByKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	var nodes []graph.Node
	for _, kind := range kinds {
		scoped, err := session.repository.ListNodesByKind(ctx, graph.NodeListQuery{
			Kinds: []graph.NodeKind{graph.NodeKind(kind)}, Visibility: graph.AllNodes})
		if err != nil {
			return nil, err
		}
		for _, entry := range scoped {
			nodes = append(nodes, entry.Node)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes, nil
}

// stateDigest digests the durable graph content: counts, file records, every
// node row, and when sweep is true the full adjacency of every node.
func (session *equivalenceSession) stateDigest(ctx context.Context, sweep bool) (string, error) {
	counts, err := session.repository.Counts(ctx)
	if err != nil {
		return "", err
	}
	files, err := session.repository.Files(ctx)
	if err != nil {
		return "", err
	}
	nodes, err := session.enumerateNodes(ctx)
	if err != nil {
		return "", err
	}
	lines := []string{renderCounts(counts), renderFiles(files)}
	for _, node := range nodes {
		lines = append(lines, renderNode(node))
	}
	if sweep {
		for _, node := range nodes {
			from, err := session.repository.EdgesFrom(ctx, node.ID)
			if err != nil {
				return "", err
			}
			to, err := session.repository.EdgesTo(ctx, node.ID)
			if err != nil {
				return "", err
			}
			lines = append(lines, fmt.Sprintf("adjacency %s from=[%s] to=[%s]",
				node.ID, renderEdges(from), renderEdges(to)))
		}
	}
	pending, err := session.repository.ReconciliationPending(ctx)
	if err != nil {
		return "", err
	}
	lines = append(lines, fmt.Sprintf("pending=%t", pending))
	return digestLines(lines), nil
}

// probeSuite renders the shared access-pattern surface: counts, files, node
// lookups, adjacency, relation pages, search, selector matching, kind
// enumeration, external federation lookups, embeddings, and metadata.
func (session *equivalenceSession) probeSuite(ctx context.Context, sweep bool) (string, error) {
	repository := session.repository
	hubID := longNodeID("hub", 0)
	populationFirst := longNodeID("population", 0)
	ambiguityProbe := longNodeID("ambiguity", 2)
	testSource := longNodeID("test", 0)
	externalQualified := externalSymbolName(0)
	externalNode := graph.Node{QualifiedName: externalQualified, Name: graph.SimpleName(externalQualified)}

	if counts, err := repository.Counts(ctx); err != nil {
		return "", err
	} else {
		session.record("counts", renderCounts(counts))
	}
	if files, err := repository.Files(ctx); err != nil {
		return "", err
	} else {
		session.record("files", renderFiles(files))
	}
	if node, err := repository.Node(ctx, hubID); err != nil {
		session.record("node-hub", renderError(err))
	} else {
		session.record("node-hub", renderNode(node))
	}
	_, err := repository.Node(ctx, "n:equivalence/missing/symbol")
	session.record("node-missing", renderError(err))
	if nodes, err := repository.EdgesFrom(ctx, hubID); err != nil {
		return "", err
	} else {
		session.record("edges-from-hub", renderEdges(nodes))
	}
	if edges, err := repository.EdgesTo(ctx, hubID); err != nil {
		return "", err
	} else {
		session.record("edges-to-hub-count", strconv.Itoa(len(edges)))
	}
	if edges, err := repository.EdgesFrom(ctx, populationFirst); err != nil {
		return "", err
	} else {
		session.record("edges-from-population-first", renderEdges(edges))
	}
	if edges, err := repository.EdgesFrom(ctx, testSource); err != nil {
		return "", err
	} else {
		session.record("edges-from-test-source", renderEdges(edges))
	}
	if page, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: hubID,
		Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeCalls}, Limit: 10}); err != nil {
		return "", err
	} else {
		session.record("relation-incoming", renderRelationPage(page))
	}
	if page, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: hubID,
		Direction: graph.OutgoingRelations, Relations: []graph.EdgeKind{graph.EdgeCalls}, Limit: 10}); err != nil {
		return "", err
	} else {
		session.record("relation-outgoing", renderRelationPage(page))
	}
	if page, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: hubID,
		Direction: graph.IncomingRelations, Relations: []graph.EdgeKind{graph.EdgeCalls}, Limit: 5}); err != nil {
		return "", err
	} else {
		session.record("relation-incoming-truncated", renderRelationPage(page))
	}
	if page, err := repository.RelationEdges(ctx, graph.RelationEdgeQuery{SubjectID: ambiguityProbe,
		Direction: graph.OutgoingRelations, Relations: []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences},
		Limit: 10}); err != nil {
		return "", err
	} else {
		session.record("relation-multi", renderRelationPage(page))
	}

	searchTerms := []struct {
		term  string
		limit int
	}{
		{term: "HubDispatch", limit: 100},
		{term: "hubdispatch", limit: 100},
		{term: "İstanbul", limit: 100},
		{term: "widgetcase", limit: 100},
		{term: "配置", limit: 100},
		{term: "ProductionTarget", limit: 3},
		{term: "ResolveAmbiguous", limit: 100},
	}
	for _, search := range searchTerms {
		if nodes, err := repository.SearchNodes(ctx, search.term, search.limit); err != nil {
			return "", err
		} else {
			session.record("search-"+search.term, fmt.Sprintf("limit=%d %s", search.limit, renderNodes(nodes)))
		}
	}

	matches := []struct {
		name    string
		request graph.NodeMatchQuery
	}{
		{name: "qualified", request: graph.NodeMatchQuery{Selector: "pkg.HubDispatch"}},
		{name: "name-folded", request: graph.NodeMatchQuery{Selector: "hubdispatch"}},
		{name: "ambiguous", request: graph.NodeMatchQuery{Selector: ambiguousPlainName}},
		{name: "substring", request: graph.NodeMatchQuery{Selector: "Dispatch"}},
		{name: "kind-filtered", request: graph.NodeMatchQuery{Selector: "HubDispatch", Kind: graph.KindFunction}},
		{name: "kind-excluded", request: graph.NodeMatchQuery{Selector: "HubDispatch", Kind: graph.KindType}},
		{name: "external", request: graph.NodeMatchQuery{Selector: externalQualified}},
		{name: "wrong-repository", request: graph.NodeMatchQuery{Selector: "pkg.HubDispatch", Repository: "elsewhere"}},
		{name: "bounded", request: graph.NodeMatchQuery{Selector: "Dispatch", Limit: 2}},
	}
	for _, match := range matches {
		if group, err := repository.MatchNodes(ctx, match.request); err != nil {
			return "", err
		} else {
			session.record("match-"+match.name, renderMatchGroup(group))
		}
	}

	lists := []struct {
		name    string
		request graph.NodeListQuery
	}{
		{name: "function-local", request: graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindFunction},
			Visibility: graph.LocalNodes, Limit: 7}},
		{name: "function-external", request: graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindFunction},
			Visibility: graph.ExternalNodes, Limit: 7}},
		{name: "function-all", request: graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindFunction},
			Visibility: graph.AllNodes, Limit: 7}},
		{name: "fragment", request: graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindFunction},
			Visibility: graph.LocalNodes, Name: "populationfunction000000", Limit: 5}},
		{name: "test-kind", request: graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindTest},
			Visibility: graph.LocalNodes}},
		{name: "external-kind", request: graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindExternal},
			Visibility: graph.ExternalNodes, Limit: 9}},
		{name: "wrong-repository", request: graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindFunction},
			Repository: "elsewhere"}},
	}
	for _, list := range lists {
		if scoped, err := repository.ListNodesByKind(ctx, list.request); err != nil {
			return "", err
		} else {
			nodes := make([]graph.Node, 0, len(scoped))
			repositoryNames := ""
			for index, entry := range scoped {
				nodes = append(nodes, entry.Node)
				if index == 0 {
					repositoryNames = entry.Repository
				}
			}
			session.record("list-"+list.name, fmt.Sprintf("repository=%s %s", repositoryNames, renderNodes(nodes)))
		}
	}

	if nodes, err := repository.ExternalNodesMatching(ctx, externalNode); err != nil {
		return "", err
	} else {
		session.record("external-nodes-matching", renderNodes(nodes))
	}
	if nodes, err := repository.ExternalNodesMatching(ctx, graph.Node{QualifiedName: ambiguousPlainName,
		Name: ambiguousPlainName}); err != nil {
		return "", err
	} else {
		session.record("external-nodes-ambiguous", renderNodes(nodes))
	}
	if edges, err := repository.ExternalEdgesTo(ctx, externalNode); err != nil {
		return "", err
	} else {
		session.record("external-edges", renderEdges(edges))
	}
	if names, err := repository.Repositories(ctx); err != nil {
		return "", err
	} else {
		session.record("repositories", strings.Join(names, ","))
	}
	if pending, err := repository.ReconciliationPending(ctx); err != nil {
		return "", err
	} else {
		session.record("pending", strconv.FormatBool(pending))
	}
	if candidates, err := repository.CandidateNodes(ctx); err != nil {
		return "", err
	} else {
		rendered := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			rendered = append(rendered, renderNode(candidate))
		}
		session.record("candidate-nodes", digestLines(rendered))
	}
	session.record("write-stats", renderWriteStats(repository.WriteStats()))
	return session.stateDigest(ctx, sweep)
}

// The equivalence corpus adds four small files to the fixture: a folding probe
// (Turkish dotted capital I, ASCII case, CJK), an orphan-producing textual
// fact, and a textual fact whose declaration arrives later.
const (
	foldingOwnerPath    = "src/equivalence/folding.go"
	orphanOwnerPath     = "src/equivalence/orphan.go"
	convergenceAPath    = "src/equivalence/convergence_a.go"
	convergenceBPath    = "src/equivalence/convergence_b.go"
	orphanTarget        = "orphan.probe/Only/Dispatch"
	orphanReplacement   = "orphan.probe/Replacement/Dispatch"
	convergenceTargetQN = "pkgConvergence.ConvergenceTarget"
)

func equivalenceNode(id string, kind graph.NodeKind, name, qualifiedName, owner string, line int) graph.Node {
	return graph.Node{ID: id, Kind: kind, Name: name, QualifiedName: qualifiedName, Language: "go",
		Location:   graph.Location{Path: owner, Line: line, Column: 1, EndLine: line},
		Properties: map[string]string{"fixture": "layoutbench", "probe": "equivalence"}, OwnerFile: owner}
}

func equivalenceFact(owner, fromID string, kind graph.EdgeKind, targetID, target string, line int) graph.Fact {
	return graph.Fact{ID: graph.FactID(owner, fromID, kind, targetID+target, line, line),
		FromID: fromID, Kind: kind, Producer: "layoutbench", TargetID: targetID, Target: target,
		Location:   graph.Location{Path: owner, Line: line, Column: 1, EndLine: line},
		Properties: map[string]string{"fixture": "layoutbench"}, OwnerFile: owner}
}

func equivalenceFileRecord(owner string, ordinal int, rows int) graph.FileRecord {
	return graph.FileRecord{Path: owner, Hash: graph.StableID("equivalence", owner, strconv.Itoa(ordinal)),
		Language: "go", Size: int64(rows), ModifiedNS: int64(ordinal) + 1, IndexedAt: "2026-01-01T00:00:00Z"}
}

func foldingFile(ordinal int) FixtureFile {
	istanbul := equivalenceNode("n:equivalence/folding/istanbul", graph.KindFunction,
		"İstanbulWidget", "pkgEquivalence.İstanbulWidget", foldingOwnerPath, 10)
	widgetCase := equivalenceNode("n:equivalence/folding/case", graph.KindType,
		"WidgetCaseProbe", "pkgEquivalence.WidgetCaseProbe", foldingOwnerPath, 20)
	cjk := equivalenceNode("n:equivalence/folding/cjk", graph.KindFunction,
		"配置入口", "pkgEquivalence.配置入口", foldingOwnerPath, 30)
	nodes := []graph.Node{istanbul, widgetCase, cjk}
	facts := []graph.Fact{
		equivalenceFact(foldingOwnerPath, istanbul.ID, graph.EdgeCalls, widgetCase.ID, "", 11),
		equivalenceFact(foldingOwnerPath, cjk.ID, graph.EdgeReferences, "", "upstream.example.org/配置/Entry", 31),
	}
	return FixtureFile{Record: equivalenceFileRecord(foldingOwnerPath, ordinal, len(nodes)+len(facts)),
		Parsed: graph.ParseResult{Nodes: nodes, Facts: facts}}
}

func orphanFile(ordinal int) FixtureFile {
	sender := equivalenceNode("n:equivalence/orphan/sender", graph.KindFunction,
		"OrphanSender", "pkgEquivalence.OrphanSender", orphanOwnerPath, 10)
	facts := []graph.Fact{equivalenceFact(orphanOwnerPath, sender.ID, graph.EdgeReferences, "", orphanTarget, 11)}
	return FixtureFile{Record: equivalenceFileRecord(orphanOwnerPath, ordinal, 2),
		Parsed: graph.ParseResult{Nodes: []graph.Node{sender}, Facts: facts}}
}

func orphanReplacementFile(ordinal int) FixtureFile {
	sender := equivalenceNode("n:equivalence/orphan/sender", graph.KindFunction,
		"OrphanSender", "pkgEquivalence.OrphanSender", orphanOwnerPath, 10)
	facts := []graph.Fact{equivalenceFact(orphanOwnerPath, sender.ID, graph.EdgeReferences, "", orphanReplacement, 11)}
	return FixtureFile{Record: equivalenceFileRecord(orphanOwnerPath, ordinal, 2),
		Parsed: graph.ParseResult{Nodes: []graph.Node{sender}, Facts: facts}}
}

func convergenceAFile(ordinal int) FixtureFile {
	probe := equivalenceNode("n:equivalence/convergence/probe", graph.KindFunction,
		"ConvergenceProbe", "pkgEquivalence.ConvergenceProbe", convergenceAPath, 10)
	facts := []graph.Fact{equivalenceFact(convergenceAPath, probe.ID, graph.EdgeCalls, "", convergenceTargetQN, 11)}
	return FixtureFile{Record: equivalenceFileRecord(convergenceAPath, ordinal, 2),
		Parsed: graph.ParseResult{Nodes: []graph.Node{probe}, Facts: facts}}
}

func convergenceBFile(ordinal int) FixtureFile {
	target := equivalenceNode("n:equivalence/convergence/target", graph.KindFunction,
		"ConvergenceTarget", convergenceTargetQN, convergenceBPath, 10)
	return FixtureFile{Record: equivalenceFileRecord(convergenceBPath, ordinal, 1),
		Parsed: graph.ParseResult{Nodes: []graph.Node{target}}}
}

// equivalenceCorpus is the shared starting corpus of the main, incremental,
// cancellation, and boundary scenarios: the fixture plus the extra probes.
func equivalenceCorpus(fixture Fixture) []FixtureFile {
	corpus := []FixtureFile{foldingFile(1), orphanFile(2), convergenceAFile(3)}
	return append(corpus, fixture.Files...)
}

func openSession(ctx context.Context, opener RepositoryOpener, directory, name string,
	log *sessionLog) (*equivalenceSession, error) {
	repository, err := opener(ctx, filepath.Join(directory, name, "graph.db"))
	if err != nil {
		return nil, err
	}
	return &equivalenceSession{scenario: name, log: log, repository: repository}, nil
}

// runMainScenario indexes the corpus, then exercises the whole read surface,
// embeddings, metadata, a later declaration that resolves a textual fact, and
// orphan external-node cleanup. It returns the baseline state digest the
// boundary scenarios converge to.
func runMainScenario(ctx context.Context, opener RepositoryOpener, directory string,
	fixture Fixture, log *sessionLog) (string, error) {
	session, err := openSession(ctx, opener, directory, "main", log)
	if err != nil {
		return "", err
	}
	defer func() { _ = session.repository.Close() }()
	repository := session.repository
	if err := repository.SetMeta(ctx, "root", equivalenceRoot); err != nil {
		return "", err
	}
	for _, file := range equivalenceCorpus(fixture) {
		if err := repository.ReplaceFile(ctx, file.Record, file.Parsed); err != nil {
			return "", fmt.Errorf("replace %s: %w", file.Record.Path, err)
		}
	}
	if err := repository.Reconcile(ctx); err != nil {
		return "", err
	}
	baselineDigest, err := session.probeSuite(ctx, true)
	if err != nil {
		return "", err
	}
	session.record("baseline-state", baselineDigest)
	baselineState, err := session.stateDigest(ctx, false)
	if err != nil {
		return "", err
	}

	// Embeddings for two candidate nodes; the second one's owner file is not
	// touched again, the first one is updated in place.
	istanbulID := "n:equivalence/folding/istanbul"
	embeddings := []semantic.Embedding{
		{NodeID: istanbulID, Model: equivalenceEmbeddingModel,
			ContentHash: graph.StableID("embedding", istanbulID, "1"),
			Vector:      []float32{0.25, -1.5, 2}, UpdatedAt: "2026-01-02T03:04:05Z"},
		{NodeID: longNodeID("population", 0), Model: equivalenceEmbeddingModel,
			ContentHash: graph.StableID("embedding", longNodeID("population", 0), "1"),
			Vector:      []float32{-0.125, 3.5}, UpdatedAt: "2026-01-02T03:04:06Z"},
	}
	for _, embedding := range embeddings {
		if err := repository.UpsertEmbedding(ctx, embedding); err != nil {
			return "", err
		}
	}
	embeddings[0].Vector = []float32{9, 8, 7, 6}
	embeddings[0].ContentHash = graph.StableID("embedding", istanbulID, "2")
	if err := repository.UpsertEmbedding(ctx, embeddings[0]); err != nil {
		return "", err
	}
	if stored, err := repository.Embeddings(ctx, equivalenceEmbeddingModel); err != nil {
		return "", err
	} else {
		session.record("embeddings", renderEmbeddings(stored))
	}
	if hashes, err := repository.EmbeddingHashes(ctx, equivalenceEmbeddingModel); err != nil {
		return "", err
	} else {
		session.record("embedding-hashes", renderHashes(hashes))
	}

	// A third embedding whose node is not a semantic candidate must be removed
	// by DeleteStaleEmbeddings while the candidate ones survive.
	staleID := "n:equivalence/noncandidate"
	staleEmbedding := semantic.Embedding{NodeID: staleID, Model: equivalenceEmbeddingModel,
		ContentHash: graph.StableID("embedding", staleID, "1"),
		Vector:      []float32{0.5}, UpdatedAt: "2026-01-02T03:04:07Z"}
	if err := repository.UpsertEmbedding(ctx, staleEmbedding); err != nil {
		return "", err
	}
	removed, err := repository.DeleteStaleEmbeddings(ctx, equivalenceEmbeddingModel)
	if err != nil {
		return "", err
	}
	session.record("stale-embeddings-removed", strconv.FormatInt(removed, 10))
	if stored, err := repository.Embeddings(ctx, equivalenceEmbeddingModel); err != nil {
		return "", err
	} else {
		session.record("embeddings-after-stale-removal", renderEmbeddings(stored))
	}
	if hashes, err := repository.EmbeddingHashes(ctx, equivalenceEmbeddingModel); err != nil {
		return "", err
	} else {
		session.record("embedding-hashes-after-stale-removal", renderHashes(hashes))
	}

	if err := repository.SetMeta(ctx, "run_id", "equivalence-run-001"); err != nil {
		return "", err
	}
	for _, key := range []string{"root", "run_id", "schema_version"} {
		value, err := repository.Meta(ctx, key)
		if err != nil {
			return "", err
		}
		session.record("meta-"+key, value)
	}

	// A later declaration converges the earlier textual fact onto the real
	// node and orphans the materialized external placeholder.
	if err := repository.ReplaceFile(ctx, convergenceBFile(4).Record, convergenceBFile(4).Parsed); err != nil {
		return "", err
	}
	if err := repository.Reconcile(ctx); err != nil {
		return "", err
	}
	if nodes, err := repository.ExternalNodesMatching(ctx, graph.Node{QualifiedName: convergenceTargetQN,
		Name: "ConvergenceTarget"}); err != nil {
		return "", err
	} else {
		session.record("convergence-external", renderNodes(nodes))
	}
	if node, err := repository.Node(ctx, "n:equivalence/convergence/target"); err != nil {
		session.record("convergence-node", renderError(err))
	} else {
		session.record("convergence-node", renderNode(node))
	}
	if edges, err := repository.EdgesFrom(ctx, "n:equivalence/convergence/probe"); err != nil {
		return "", err
	} else {
		session.record("convergence-edges", renderEdges(edges))
	}

	// Replacing the orphan file with a different textual target orphans the
	// old external node; reconciliation cleanup must remove it.
	if err := repository.ReplaceFile(ctx, orphanReplacementFile(5).Record, orphanReplacementFile(5).Parsed); err != nil {
		return "", err
	}
	if err := repository.Reconcile(ctx); err != nil {
		return "", err
	}
	oldExternal := graph.Node{QualifiedName: orphanTarget, Name: graph.SimpleName(orphanTarget)}
	if nodes, err := repository.ExternalNodesMatching(ctx, oldExternal); err != nil {
		return "", err
	} else {
		session.record("orphan-external", renderNodes(nodes))
	}
	newExternal := graph.Node{QualifiedName: orphanReplacement, Name: graph.SimpleName(orphanReplacement)}
	if nodes, err := repository.ExternalNodesMatching(ctx, newExternal); err != nil {
		return "", err
	} else {
		session.record("orphan-replacement-external", renderNodes(nodes))
	}
	session.scenario = "main-final"
	finalDigest, err := session.stateDigest(ctx, true)
	if err != nil {
		return "", err
	}
	session.record("final-state", finalDigest)
	return baselineState, nil
}

// copyProperties deep-copies a property map so scenario variants never mutate
// the shared fixture both adapters index from; callers add keys to the copy, so
// it is never nil.
func copyProperties(properties map[string]string) map[string]string {
	copied := maps.Clone(properties)
	if copied == nil {
		copied = make(map[string]string)
	}
	return copied
}

// editedPopulationFile shifts locations and properties of the first population
// file without changing its identity, so an incremental re-index must converge
// back to the original rows once the original content returns.
func editedPopulationFile(file FixtureFile) FixtureFile {
	parsed := graph.ParseResult{Nodes: make([]graph.Node, len(file.Parsed.Nodes)),
		Facts: make([]graph.Fact, len(file.Parsed.Facts))}
	for index, node := range file.Parsed.Nodes {
		node.Location.Line += 7
		node.Location.EndLine += 7
		node.Properties = copyProperties(node.Properties)
		node.Properties["variation"] = graph.StableID("edited", node.ID)
		parsed.Nodes[index] = node
	}
	for index, fact := range file.Parsed.Facts {
		fact.Location.Line += 7
		fact.Properties = copyProperties(fact.Properties)
		fact.Properties["variation"] = graph.StableID("edited", fact.ID)
		parsed.Facts[index] = fact
	}
	record := file.Record
	record.Hash = graph.StableID("equivalence-edited", file.Record.Path)
	record.Size++
	return FixtureFile{Record: record, Parsed: parsed}
}

// branchVariants replace the hub and shared files with branch content and drop
// the test file, then the scenario restores the originals.
func branchHubFile(file FixtureFile) FixtureFile {
	hub := file.Parsed.Nodes[0]
	hub.QualifiedName = "pkgBranch.HubDispatch"
	hub.Properties = copyProperties(hub.Properties)
	hub.Properties["branch"] = "spike"
	return FixtureFile{Record: func() graph.FileRecord {
		record := file.Record
		record.Hash = graph.StableID("equivalence-branch", file.Record.Path)
		return record
	}(), Parsed: graph.ParseResult{Nodes: []graph.Node{hub, file.Parsed.Nodes[1]}}}
}

func branchSharedFile(file FixtureFile) FixtureFile {
	probe := file.Parsed.Nodes[1]
	productionTarget := file.Parsed.Nodes[2]
	branchOnly := equivalenceNode("n:equivalence/shared/branch-only", graph.KindFunction,
		"BranchOnlySymbol", "pkgShared.BranchOnlySymbol", file.Record.Path, 40)
	nodes := []graph.Node{probe, productionTarget, branchOnly}
	facts := make([]graph.Fact, len(file.Parsed.Facts))
	copy(facts, file.Parsed.Facts)
	return FixtureFile{Record: func() graph.FileRecord {
		record := file.Record
		record.Hash = graph.StableID("equivalence-branch", file.Record.Path)
		return record
	}(), Parsed: graph.ParseResult{Nodes: nodes, Facts: facts}}
}

func runIncrementalScenario(ctx context.Context, opener RepositoryOpener, directory string,
	fixture Fixture, log *sessionLog) error {
	session, err := openSession(ctx, opener, directory, "incremental", log)
	if err != nil {
		return err
	}
	defer func() { _ = session.repository.Close() }()
	repository := session.repository
	corpus := equivalenceCorpus(fixture)
	if err := repository.SetMeta(ctx, "root", equivalenceRoot); err != nil {
		return err
	}
	for _, file := range corpus {
		if err := repository.ReplaceFile(ctx, file.Record, file.Parsed); err != nil {
			return err
		}
	}
	if err := repository.Reconcile(ctx); err != nil {
		return err
	}
	digestBefore, err := session.stateDigest(ctx, true)
	if err != nil {
		return err
	}
	digestBeforeUnswept, err := session.stateDigest(ctx, false)
	if err != nil {
		return err
	}
	session.record("baseline", digestBefore)

	// ReplaceOwner with identical content must leave the durable state
	// unchanged; it exercises the owner-scoped write path.
	folding := corpus[0]
	if err := repository.ReplaceOwner(ctx, folding.Record.Path, folding.Parsed); err != nil {
		return err
	}
	if err := repository.Reconcile(ctx); err != nil {
		return err
	}
	digestAfterOwner, err := session.stateDigest(ctx, false)
	if err != nil {
		return err
	}
	session.record("replace-owner-unchanged", strconv.FormatBool(digestAfterOwner == digestBeforeUnswept))

	populationFile, err := fileByPath(fixture, populationOwnerPath(0))
	if err != nil {
		return err
	}
	callersFile, err := fileByPath(fixture, callersOwnerPath(fixturePopulationFileCount(fixture)))
	if err != nil {
		return err
	}

	edited := editedPopulationFile(populationFile)
	if err := repository.ReplaceFile(ctx, edited.Record, edited.Parsed); err != nil {
		return err
	}
	if err := repository.Reconcile(ctx); err != nil {
		return err
	}
	if edges, err := repository.EdgesFrom(ctx, longNodeID("population", 0)); err != nil {
		return err
	} else {
		session.record("edited-edges", renderEdges(edges))
	}

	if err := repository.RemoveFiles(ctx, []string{callersFile.Record.Path}); err != nil {
		return err
	}
	if err := repository.Reconcile(ctx); err != nil {
		return err
	}
	if _, err := repository.Node(ctx, longNodeID("callers", 0)); err != nil {
		session.record("deleted-caller", renderError(err))
	} else {
		session.record("deleted-caller", "present")
	}
	if edges, err := repository.EdgesTo(ctx, longNodeID("hub", 0)); err != nil {
		return err
	} else {
		session.record("deleted-hub-incoming", strconv.Itoa(len(edges)))
	}

	// Restoring the original content of both files must converge the graph
	// back to the exact pre-edit state.
	if err := repository.ReplaceFile(ctx, populationFile.Record, populationFile.Parsed); err != nil {
		return err
	}
	if err := repository.ReplaceFile(ctx, callersFile.Record, callersFile.Parsed); err != nil {
		return err
	}
	if err := repository.Reconcile(ctx); err != nil {
		return err
	}
	digestRestored, err := session.stateDigest(ctx, true)
	if err != nil {
		return err
	}
	session.record("restored-state", digestRestored)
	session.record("restored-converged", strconv.FormatBool(digestRestored == digestBefore))

	moduleCount := fixturePopulationFileCount(fixture)
	hubFile, err := fileByPath(fixture, hubOwnerPath(moduleCount+3))
	if err != nil {
		return err
	}
	sharedFile, err := fileByPath(fixture, sharedOwnerPath(moduleCount+2))
	if err != nil {
		return err
	}
	testFile, err := fileByPath(fixture, testOwnerPath(moduleCount+4))
	if err != nil {
		return err
	}

	if err := repository.ReplaceFile(ctx, branchHubFile(hubFile).Record, branchHubFile(hubFile).Parsed); err != nil {
		return err
	}
	if err := repository.ReplaceFile(ctx, branchSharedFile(sharedFile).Record, branchSharedFile(sharedFile).Parsed); err != nil {
		return err
	}
	if err := repository.RemoveFiles(ctx, []string{testFile.Record.Path}); err != nil {
		return err
	}
	if err := repository.Reconcile(ctx); err != nil {
		return err
	}
	if group, err := repository.MatchNodes(ctx, graph.NodeMatchQuery{Selector: "pkgShared.BranchOnlySymbol"}); err != nil {
		return err
	} else {
		session.record("branch-match", renderMatchGroup(group))
	}
	branchDigest, err := session.stateDigest(ctx, false)
	if err != nil {
		return err
	}
	session.record("branch-state", branchDigest)

	// Switching back to the original branch content converges again.
	if err := repository.ReplaceFile(ctx, hubFile.Record, hubFile.Parsed); err != nil {
		return err
	}
	if err := repository.ReplaceFile(ctx, sharedFile.Record, sharedFile.Parsed); err != nil {
		return err
	}
	if err := repository.ReplaceFile(ctx, testFile.Record, testFile.Parsed); err != nil {
		return err
	}
	if err := repository.Reconcile(ctx); err != nil {
		return err
	}
	digestBack, err := session.stateDigest(ctx, true)
	if err != nil {
		return err
	}
	session.record("branch-restored-state", digestBack)
	session.record("branch-restored-converged", strconv.FormatBool(digestBack == digestBefore))
	return nil
}

// fixturePopulationFileCount derives the fixture's population fan-out from
// the fixture itself (population files are the /generated.go module files),
// so scenario helpers locate structural files by path without restating the
// generator's formula.
func fixturePopulationFileCount(fixture Fixture) int {
	count := 0
	for _, file := range fixture.Files {
		if strings.HasSuffix(file.Record.Path, "/generated.go") {
			count++
		}
	}
	return count
}

// fileByPath locates one fixture file by owner path regardless of the seed's
// insertion order; a miss is a scenario bug, so it fails loudly.
func fileByPath(fixture Fixture, path string) (FixtureFile, error) {
	for _, file := range fixture.Files {
		if file.Record.Path == path {
			return file, nil
		}
	}
	return FixtureFile{}, fmt.Errorf("fixture has no file at path %s", path)
}

// runCancellationScenario cancels ReplaceFile mid-flight with shrinking
// deadlines, verifies each attempt is atomic (the file is either fully present
// or fully absent), normalizes the state, and proves convergence back to the
// baseline digest. Timing-dependent outcomes are deliberately not recorded.
func runCancellationScenario(ctx context.Context, opener RepositoryOpener, directory string,
	fixture Fixture, log *sessionLog) error {
	session, err := openSession(ctx, opener, directory, "cancellation", log)
	if err != nil {
		return err
	}
	defer func() { _ = session.repository.Close() }()
	repository := session.repository
	if err := repository.SetMeta(ctx, "root", equivalenceRoot); err != nil {
		return err
	}
	for _, file := range equivalenceCorpus(fixture) {
		if err := repository.ReplaceFile(ctx, file.Record, file.Parsed); err != nil {
			return err
		}
	}
	if err := repository.Reconcile(ctx); err != nil {
		return err
	}
	digestBefore, err := session.stateDigest(ctx, false)
	if err != nil {
		return err
	}

	owner := "src/equivalence/cancellation/big.go"
	const rowCount = 4000
	parsed := graph.ParseResult{Nodes: make([]graph.Node, 0, rowCount), Facts: make([]graph.Fact, 0, rowCount)}
	for index := range rowCount {
		node := equivalenceNode(fmt.Sprintf("n:equivalence/cancellation/node%05d", index), graph.KindFunction,
			fmt.Sprintf("CancellationNode%05d", index), fmt.Sprintf("pkgCancellation.CancellationNode%05d", index),
			owner, index+1)
		parsed.Nodes = append(parsed.Nodes, node)
		next := fmt.Sprintf("n:equivalence/cancellation/node%05d", (index+1)%rowCount)
		parsed.Facts = append(parsed.Facts, equivalenceFact(owner, node.ID, graph.EdgeCalls, next, "", index+1))
	}
	record := equivalenceFileRecord(owner, 9, rowCount*2)

	atomic := true
	for attempt := 0; attempt <= 10; attempt++ {
		var attemptCtx context.Context
		if attempt == 10 {
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			attemptCtx = canceled
		} else {
			var cancel context.CancelFunc
			attemptCtx, cancel = context.WithTimeout(ctx, time.Duration(2048>>attempt)*time.Microsecond)
			defer cancel()
		}
		_ = repository.ReplaceFile(attemptCtx, record, parsed)
		files, err := repository.Files(ctx)
		if err != nil {
			return err
		}
		_, nodeErr := repository.Node(ctx, "n:equivalence/cancellation/node00000")
		committed, nodePresent := false, nodeErr == nil
		if _, found := files[owner]; found {
			committed = true
		}
		if (committed && !nodePresent) || (!committed && nodePresent) {
			atomic = false
		}
		if committed {
			if err := repository.RemoveFiles(ctx, []string{owner}); err != nil {
				return err
			}
			if err := repository.Reconcile(ctx); err != nil {
				return err
			}
		}
	}
	session.record("attempts", "11")
	session.record("atomic", strconv.FormatBool(atomic))
	digestAfter, err := session.stateDigest(ctx, false)
	if err != nil {
		return err
	}
	session.record("converged", strconv.FormatBool(digestAfter == digestBefore))
	return nil
}

// scriptStep is one durable action of the canonical indexing script together
// with the indexer boundary it completes.
type scriptStep struct {
	boundary indexer.BoundaryKind
	run      func(ctx context.Context, repository EquivalenceRepository) error
}

// canonicalScript is the resume-equivalence script: index every corpus file,
// remove one file, restore it, reconcile, and persist metadata. Its final
// state equals the main-scenario baseline.
func canonicalScript(corpus []FixtureFile, removeFile FixtureFile) []scriptStep {
	steps := make([]scriptStep, 0, len(corpus)+6)
	for _, file := range corpus {
		steps = append(steps, scriptStep{boundary: indexer.BoundaryFilePersisted,
			run: func(ctx context.Context, repository EquivalenceRepository) error {
				return repository.ReplaceFile(ctx, file.Record, file.Parsed)
			}})
	}
	steps = append(steps, scriptStep{boundary: indexer.BoundaryWorkspacePersisted,
		run: func(context.Context, EquivalenceRepository) error { return nil }})
	steps = append(steps, scriptStep{boundary: indexer.BoundaryFilesRemoved,
		run: func(ctx context.Context, repository EquivalenceRepository) error {
			return repository.RemoveFiles(ctx, []string{removeFile.Record.Path})
		}})
	steps = append(steps, scriptStep{boundary: indexer.BoundaryFilePersisted,
		run: func(ctx context.Context, repository EquivalenceRepository) error {
			return repository.ReplaceFile(ctx, removeFile.Record, removeFile.Parsed)
		}})
	steps = append(steps, scriptStep{boundary: indexer.BoundaryReconciliationBatch,
		run: func(ctx context.Context, repository EquivalenceRepository) error {
			_, err := repository.ReconcileWithStats(ctx, nil)
			return err
		}})
	steps = append(steps, scriptStep{boundary: indexer.BoundaryMetadataPersisted,
		run: func(ctx context.Context, repository EquivalenceRepository) error {
			return repository.SetMeta(ctx, "equivalence_status", "complete")
		}})
	return steps
}

// executeScript runs the script; interrupted reports that it stopped right
// after the first boundary of the requested kind, simulating a crash there.
func executeScript(ctx context.Context, repository EquivalenceRepository, steps []scriptStep,
	stopAt indexer.BoundaryKind) (interrupted bool, err error) {
	if err := repository.SetMeta(ctx, "root", equivalenceRoot); err != nil {
		return false, err
	}
	for _, step := range steps {
		if step.boundary == indexer.BoundaryReconciliationBatch {
			_, reconcileErr := repository.ReconcileWithStats(ctx, func(graph.ReconciliationStats) error {
				if stopAt == indexer.BoundaryReconciliationBatch {
					return errEquivalenceInterrupt
				}
				return nil
			})
			if errors.Is(reconcileErr, errEquivalenceInterrupt) {
				return true, nil
			}
			if reconcileErr != nil {
				return false, reconcileErr
			}
		} else if err := step.run(ctx, repository); err != nil {
			return false, err
		}
		if step.boundary == stopAt {
			return true, nil
		}
	}
	return false, nil
}

// runBoundaryScenarios interrupts a canonical indexing run at every indexer
// boundary kind, reopens the database, resumes, and requires convergence to
// the uninterrupted canonical state (and to the main-scenario baseline for the
// probe-scale fixture). The reconciliation boundary uses the batch fixture so
// the interruption lands between committed batches.
func runBoundaryScenarios(ctx context.Context, opener RepositoryOpener, directory string,
	fixture, batchFixture Fixture, baselineDigest string, log *sessionLog) error {
	kinds := []indexer.BoundaryKind{
		indexer.BoundaryFilePersisted,
		indexer.BoundaryWorkspacePersisted,
		indexer.BoundaryFilesRemoved,
		indexer.BoundaryMetadataPersisted,
	}
	for _, kind := range kinds {
		name := "boundary-" + string(kind)
		removeFile, err := callersRemovalFile(fixture)
		if err != nil {
			return err
		}
		if err := runBoundaryScenario(ctx, opener, directory, name, kind,
			equivalenceCorpus(fixture), removeFile, true, baselineDigest, log); err != nil {
			return fmt.Errorf("boundary %s: %w", kind, err)
		}
	}
	batchRemoveFile, err := callersRemovalFile(batchFixture)
	if err != nil {
		return err
	}
	return runBoundaryScenario(ctx, opener, directory, "boundary-reconciliation_batch",
		indexer.BoundaryReconciliationBatch, batchFixture.Files,
		batchRemoveFile, false, "", log)
}

// callersRemovalFile picks the hub fan-in file the canonical script removes
// and restores, located by path so it does not depend on insertion order.
func callersRemovalFile(fixture Fixture) (FixtureFile, error) {
	return fileByPath(fixture, callersOwnerPath(fixturePopulationFileCount(fixture)))
}

func runBoundaryScenario(ctx context.Context, opener RepositoryOpener, directory, name string,
	stopAt indexer.BoundaryKind, corpus []FixtureFile, removeFile FixtureFile, probeScale bool,
	baselineDigest string, log *sessionLog) error {
	steps := canonicalScript(corpus, removeFile)

	// Canonical uninterrupted run.
	canonicalSession, err := openSession(ctx, opener, directory, name+"-canonical", log)
	if err != nil {
		return err
	}
	if _, err := executeScript(ctx, canonicalSession.repository, steps, ""); err != nil {
		_ = canonicalSession.repository.Close()
		return err
	}
	canonicalDigest, err := canonicalSession.stateDigest(ctx, false)
	if err != nil {
		_ = canonicalSession.repository.Close()
		return err
	}
	if err := canonicalSession.repository.Close(); err != nil {
		return err
	}

	// Interrupted run: stop at the first boundary of the requested kind,
	// close as if the process died, reopen, and resume to completion.
	interruptedSession, err := openSession(ctx, opener, directory, name+"-interrupted", log)
	if err != nil {
		return err
	}
	interrupted, err := executeScript(ctx, interruptedSession.repository, steps, stopAt)
	if err != nil {
		_ = interruptedSession.repository.Close()
		return err
	}
	if err := interruptedSession.repository.Close(); err != nil {
		return err
	}
	resumedRepository, err := opener(ctx, filepath.Join(directory, name+"-interrupted", "graph.db"))
	if err != nil {
		return err
	}
	if _, err := executeScript(ctx, resumedRepository, steps, ""); err != nil {
		_ = resumedRepository.Close()
		return err
	}
	completion := &equivalenceSession{scenario: name, log: log, repository: resumedRepository}
	resumedDigest, err := completion.stateDigest(ctx, false)
	if err != nil {
		_ = resumedRepository.Close()
		return err
	}
	if err := resumedRepository.Close(); err != nil {
		return err
	}

	completion.record("interrupted", strconv.FormatBool(interrupted))
	completion.record("canonical-state", canonicalDigest)
	completion.record("resumed-state", resumedDigest)
	completion.record("resume-converged", strconv.FormatBool(canonicalDigest == resumedDigest))
	if probeScale {
		completion.record("matches-main-baseline", strconv.FormatBool(canonicalDigest == baselineDigest))
	}
	return nil
}
