package detailprofile

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/projectconfig"
)

type Profile string

const (
	ProfileFull       Profile = "full"
	ProfileStructural Profile = "structural-v1"
	ProfileScopedFull Profile = "scoped-full-v1"
)

type CapabilitySet map[Capability]bool

func (s CapabilitySet) Has(capability Capability) bool { return s[capability] }

func ProfileCapabilities(profile Profile) CapabilitySet {
	result := CapabilitySet{}
	switch profile {
	case ProfileFull:
		for _, capability := range capabilities {
			result[capability] = true
		}
	case ProfileStructural, ProfileScopedFull:
		for _, capability := range []Capability{
			CapabilityStructural, CapabilityTransportFlow, CapabilityTestCoverage,
			CapabilityGodotComposition, CapabilityGodotInteraction,
			CapabilityCatalogTopology, CapabilitySemanticReuse, CapabilitySourceLookup,
		} {
			result[capability] = true
		}
	}
	return result
}

type Options struct {
	Profile         Profile
	Membership      projectconfig.IndexScope
	FullDetailRoots []string
}

type Decision struct {
	Profile  Profile `json:"profile"`
	Excluded bool    `json:"excluded"`
	Reason   string  `json:"reason"`
}

type Projector struct{ options Options }

func NewProjector(options Options) *Projector { return &Projector{options: options} }

func (p *Projector) SemanticKey() string {
	roots := append([]string(nil), p.options.FullDetailRoots...)
	sort.Strings(roots)
	return "detail-profile-v1:" + string(p.options.Profile) + ":" + p.options.Membership.SemanticKey() + ":" + strings.Join(roots, ",")
}

func (p *Projector) Project(ctx context.Context, input parserapi.Input, parsed graph.ParseResult) (graph.ParseResult, Decision, error) {
	if err := ctx.Err(); err != nil {
		return graph.ParseResult{}, Decision{}, err
	}
	if !p.options.Membership.Allows(input.Path) {
		return graph.ParseResult{}, Decision{Profile: p.options.Profile, Excluded: true, Reason: "excluded by index scope before fidelity projection"}, nil
	}
	profile := p.options.Profile
	if profile == ProfileScopedFull {
		profile = ProfileStructural
		for _, root := range p.options.FullDetailRoots {
			root = strings.Trim(strings.TrimSpace(root), "/")
			if root == "." || input.Path == root || strings.HasPrefix(input.Path, root+"/") {
				profile = ProfileFull
				break
			}
		}
	}
	result, err := project(profile, parsed)
	return result, Decision{Profile: profile, Reason: "profile selected after index-scope membership"}, err
}

func (p *Projector) Transform(ctx context.Context, input parserapi.Input, parsed graph.ParseResult) (graph.ParseResult, error) {
	result, decision, err := p.Project(ctx, input, parsed)
	if err != nil {
		return graph.ParseResult{}, err
	}
	if decision.Excluded {
		return graph.ParseResult{}, fmt.Errorf("%s: detail projection received path excluded by index scope", input.Path)
	}
	if err := ValidateResolution(parsed, result); err != nil {
		return graph.ParseResult{}, fmt.Errorf("%s: %w", input.Path, err)
	}
	return result, nil
}

func project(profile Profile, parsed graph.ParseResult) (graph.ParseResult, error) {
	switch profile {
	case ProfileFull:
		return parsed, nil
	case ProfileStructural:
		return projectStructural(parsed), nil
	default:
		return graph.ParseResult{}, fmt.Errorf("unknown detail profile %q", profile)
	}
}

func projectStructural(parsed graph.ParseResult) graph.ParseResult {
	nodes := make(map[string]graph.Node, len(parsed.Nodes))
	keepNode := make(map[string]bool, len(parsed.Nodes))
	for _, node := range parsed.Nodes {
		nodes[node.ID] = node
		keepNode[node.ID] = node.Kind != graph.KindVariable && node.Kind != graph.KindParameter
	}
	keptFacts := make([]graph.Fact, 0, len(parsed.Facts))
	for _, fact := range parsed.Facts {
		if !keepStructuralFact(fact, nodes) {
			continue
		}
		keptFacts = append(keptFacts, fact)
		if fact.FromID != "" {
			keepNode[fact.FromID] = true
		}
		if fact.TargetID != "" {
			if _, local := nodes[fact.TargetID]; local {
				keepNode[fact.TargetID] = true
			}
		}
	}
	// Named retained facts can resolve to declarations owned by this result.
	// Keep every local candidate they saw so projection cannot manufacture a
	// unique match from a formerly ambiguous selector.
	for _, fact := range keptFacts {
		if fact.Source != "" {
			preserveNamedCandidates(parsed.Nodes, keepNode, fact.Source, fact.SourceKind)
		}
		if fact.Target != "" {
			preserveNamedCandidates(parsed.Nodes, keepNode, fact.Target, fact.TargetKind)
		}
	}
	result := graph.ParseResult{Facts: keptFacts, Diagnostics: append([]graph.Diagnostic(nil), parsed.Diagnostics...)}
	for _, node := range parsed.Nodes {
		if keepNode[node.ID] {
			result.Nodes = append(result.Nodes, node)
		}
	}
	return result
}

func keepStructuralFact(fact graph.Fact, nodes map[string]graph.Node) bool {
	switch fact.Kind {
	case graph.EdgeAssigns, graph.EdgeReturns, graph.EdgePasses,
		graph.EdgeReturnsError, graph.EdgePropagatesError, graph.EdgeHandlesError,
		graph.EdgeWrapsError, graph.EdgePanics, graph.EdgeRecovers, graph.EdgeDefers:
		return false
	case graph.EdgeReads, graph.EdgeWrites:
		kind := fact.TargetKind
		if target, ok := nodes[fact.TargetID]; ok {
			kind = target.Kind
		}
		return graph.IsDataResourceKind(kind)
	case graph.EdgeDeclares, graph.EdgeContains:
		if target, ok := nodes[fact.TargetID]; ok {
			return target.Kind != graph.KindVariable && target.Kind != graph.KindParameter
		}
	}
	return true
}

func preserveNamedCandidates(nodes []graph.Node, keep map[string]bool, selector string, kind graph.NodeKind) {
	for _, node := range nodes {
		if kind != "" && node.Kind != kind {
			continue
		}
		if node.ID == selector || node.QualifiedName == selector || node.Name == selector {
			keep[node.ID] = true
		}
	}
}

// ValidateClosure validates a whole projected corpus, not one parser result,
// because exact targets can legitimately be owned by another source file.
func ValidateClosure(results []graph.ParseResult) error {
	nodes := map[string]bool{}
	for _, result := range results {
		for _, node := range result.Nodes {
			nodes[node.ID] = true
		}
	}
	for _, result := range results {
		for _, fact := range result.Facts {
			if fact.FromID != "" && !nodes[fact.FromID] {
				return fmt.Errorf("fact %q has missing exact source %q", fact.ID, fact.FromID)
			}
			if fact.TargetID != "" && !nodes[fact.TargetID] {
				return fmt.Errorf("fact %q has missing exact target %q", fact.ID, fact.TargetID)
			}
		}
	}
	return nil
}

// ValidateResolution rejects any retained named locator whose exact local
// candidate set changed. This is intentionally stricter than query-time
// declaration preference: a benchmark candidate cannot claim that omitted
// evidence was never part of ambiguity.
func ValidateResolution(full, projected graph.ParseResult) error {
	projectedFacts := map[string]bool{}
	for _, fact := range projected.Facts {
		projectedFacts[fact.ID] = true
	}
	for _, fact := range full.Facts {
		if !projectedFacts[fact.ID] {
			continue
		}
		for _, locator := range []struct {
			value string
			kind  graph.NodeKind
		}{{fact.Source, fact.SourceKind}, {fact.Target, fact.TargetKind}} {
			if locator.value == "" {
				continue
			}
			before := matchingNodeIDs(full.Nodes, locator.value, locator.kind)
			after := matchingNodeIDs(projected.Nodes, locator.value, locator.kind)
			if strings.Join(before, "\x00") != strings.Join(after, "\x00") {
				return fmt.Errorf("fact %q resolution drift for %q: %v -> %v", fact.ID, locator.value, before, after)
			}
		}
	}
	return nil
}

func matchingNodeIDs(nodes []graph.Node, selector string, kind graph.NodeKind) []string {
	var result []string
	for _, node := range nodes {
		if kind != "" && node.Kind != kind {
			continue
		}
		if node.ID == selector || node.QualifiedName == selector || node.Name == selector {
			result = append(result, node.ID)
		}
	}
	sort.Strings(result)
	return result
}

type CapabilityError struct {
	Operation      string       `json:"operation"`
	Missing        []Capability `json:"missing_capabilities"`
	RebuildProfile Profile      `json:"rebuild_profile"`
}

func (e *CapabilityError) Error() string {
	names := make([]string, len(e.Missing))
	for index, capability := range e.Missing {
		names[index] = string(capability)
	}
	return fmt.Sprintf("index lacks capability %s for %s; rebuild with profile %s", strings.Join(names, ","), e.Operation, e.RebuildProfile)
}

func RequireCapabilities(available CapabilitySet, operation string) error {
	var required []Capability
	for _, candidate := range Matrix().Operations {
		if candidate.Name == operation {
			required = candidate.Capabilities
			break
		}
	}
	if len(required) == 0 {
		return fmt.Errorf("unknown capability-gated operation %q", operation)
	}
	var missing []Capability
	for _, capability := range required {
		if !available.Has(capability) {
			missing = append(missing, capability)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return &CapabilityError{Operation: operation, Missing: missing, RebuildProfile: ProfileFull}
}

func RunOperation(available CapabilitySet, operation string, repositoryRead func() error) error {
	if err := RequireCapabilities(available, operation); err != nil {
		return err
	}
	return repositoryRead()
}

type FederatedCapabilityError struct {
	Operation          string   `json:"operation"`
	UnsupportedMembers []string `json:"unsupported_members"`
	Cause              error    `json:"-"`
}

func (e *FederatedCapabilityError) Error() string {
	return fmt.Sprintf("federation members %s do not support %s: %v", strings.Join(e.UnsupportedMembers, ","), e.Operation, e.Cause)
}

func (e *FederatedCapabilityError) Unwrap() error { return e.Cause }

func RequireFederated(members map[string]CapabilitySet, operation string) error {
	var unsupported []string
	var cause error
	for member, available := range members {
		if err := RequireCapabilities(available, operation); err != nil {
			unsupported = append(unsupported, member)
			if cause == nil {
				cause = err
			}
		}
	}
	if len(unsupported) == 0 {
		return nil
	}
	sort.Strings(unsupported)
	return &FederatedCapabilityError{Operation: operation, UnsupportedMembers: unsupported, Cause: cause}
}
