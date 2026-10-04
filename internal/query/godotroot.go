package query

import (
	"context"
	"errors"
	"fmt"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/parser/godot/godotid"
)

// The Godot report surface shares the identity vocabulary with the parser that
// produced the graph rather than restating it. A Godot resource node is named by
// its path with the extension dropped, and a res:// reference is relative to its
// own project; a second copy of those two rules here is exactly how a selector
// stops agreeing with the identity it is meant to name.

// maxGodotProjects bounds the project.godot enumeration behind a res:// selector.
// A res:// path is relative to one project, so resolution has to see every
// project to prove which one holds it; a truncated list could not prove that, so
// hitting the bound is refused rather than answered from the window.
const maxGodotProjects = 256

// godotContainerRoot reports whether a root owns no Godot evidence of its own.
// A file, a script module, and a class are declaration containers: every
// composition and interaction fact about them is recorded on something they
// declare - a scene on its file, a signal connection on the method that makes
// it - so reading only the root's own edges reports zero for a selector that
// names a real, fully wired thing.
//
// A scene is not a container in this sense even though it declares its tree: it
// carries its own instantiation and script evidence, and its inbound question
// ("which scenes instantiate this scene?") is about the scene rather than about
// each of its nodes.
func godotContainerRoot(node graph.Node) bool {
	switch node.Kind {
	case graph.KindFile, graph.KindModule, graph.KindClass:
		return !node.External
	default:
		return false
	}
}

// godotEvidence is the set of nodes one Godot report reads edges from: the root,
// the scene nodes it declares, and, for a container root, the declarations it
// holds.
type godotEvidence struct {
	root       graph.Node
	sceneNodes []graph.Node
	members    []graph.Node
	truncated  bool
	container  bool
	// all holds the root, its scene nodes, and its members in that order, with
	// ids indexing them, so a report that tests every edge endpoint against the
	// set does not rebuild it per edge.
	all []graph.Node
	ids map[string]bool
}

// seal computes the evidence set once the declarations have been collected.
func (e *godotEvidence) seal() {
	e.all = make([]graph.Node, 0, 1+len(e.sceneNodes)+len(e.members))
	e.all = append(e.all, e.root)
	e.all = append(e.all, e.sceneNodes...)
	e.all = append(e.all, e.members...)
	e.ids = make(map[string]bool, len(e.all))
	for _, node := range e.all {
		e.ids[node.ID] = true
	}
}

// sources lists the nodes whose outgoing edges the report reads, root first.
func (e godotEvidence) sources() []graph.Node { return e.all }

// targets lists the nodes whose incoming edges the report reads. A container
// root is reached through its declarations, so they answer the inbound question
// on its behalf; every other root answers for itself, which keeps a scene's
// inbound section the scene's own.
func (e godotEvidence) targets() []graph.Node {
	if !e.container {
		return []graph.Node{e.root}
	}
	return e.sources()
}

// internal reports whether a node is part of this report's own evidence. An
// incoming edge from one of a container root's own declarations is internal
// wiring, already reported outbound, rather than something outside reaching in.
func (e godotEvidence) internal(id string) bool { return e.container && e.ids[id] }

// aggregation describes the member expansion for the report, or nil for a root
// that owns its own evidence so a scene's report never carries an empty section.
func (e godotEvidence) aggregation() *MemberAggregation {
	if !e.container || (len(e.members) == 0 && !e.truncated) {
		return nil
	}
	return &MemberAggregation{Relation: memberRelation, Members: e.members, Truncated: e.truncated}
}

// godotEvidenceFor resolves the selector and collects the declarations whose
// edges the report reads. Resolution errors (ErrNotFound, *AmbiguousError)
// propagate unchanged and never yield a partial report.
func (s *Service) godotEvidenceFor(ctx context.Context, selector string, kind graph.NodeKind, depth, limit int) (godotEvidence, error) {
	root, err := s.resolveGodotRoot(ctx, selector, kind)
	if err != nil {
		return godotEvidence{}, err
	}
	evidence := godotEvidence{root: root, sceneNodes: []graph.Node{},
		container: godotContainerRoot(root)}
	declaresTree := root.Kind == graph.KindGodotScene || root.Kind == graph.KindGodotSceneNode
	if !declaresTree && !evidence.container {
		evidence.seal()
		return evidence, nil
	}
	traversal, err := s.Neighborhood(ctx, root.ID, "", depth, Outgoing,
		[]graph.EdgeKind{graph.EdgeDeclares}, limit)
	if err != nil {
		return godotEvidence{}, err
	}
	for _, reached := range traversal.Nodes {
		switch {
		case reached.Depth == 0:
		case reached.Node.Kind == graph.KindGodotSceneNode:
			evidence.sceneNodes = append(evidence.sceneNodes, reached.Node)
		case evidence.container && !reached.Node.External:
			evidence.members = append(evidence.members, reached.Node)
		}
	}
	evidence.truncated = traversal.Truncated
	evidence.seal()
	return evidence, nil
}

// resolveGodotRoot resolves a Godot report's selector, accepting the Godot
// resource path forms a caller actually holds alongside the canonical identity.
//
// Textual resolution runs first and unchanged, so an identity, an autoload name,
// a node ID, and a script symbol keep resolving exactly as before, and an
// ambiguity is reported rather than reinterpreted. Only an outright
// ErrNotFound is retried as a resource path, and a path that matches nothing
// keeps the original error for the selector the caller typed.
func (s *Service) resolveGodotRoot(ctx context.Context, selector string, kind graph.NodeKind) (graph.Node, error) {
	node, err := s.ResolveKind(ctx, selector, kind)
	if err == nil || !errors.Is(err, ErrNotFound) {
		return node, err
	}
	resolved, found, pathErr := s.resolveGodotPath(ctx, selector, kind)
	switch {
	case pathErr != nil:
		return graph.Node{}, pathErr
	case found:
		return resolved, nil
	default:
		return graph.Node{}, err
	}
}

// resolveGodotPath reads the selector as a Godot resource path and resolves the
// identity it canonicalizes to. The second result reports whether the selector
// was a resource path that named exactly one node; an error is either a real
// failure or an *AmbiguousError naming the projects that both hold the path.
func (s *Service) resolveGodotPath(ctx context.Context, selector string, kind graph.NodeKind) (graph.Node, bool, error) {
	selector = strings.TrimSpace(selector)
	// A uid:// alias is declared as a node under its own name, so textual
	// resolution already owns it and it carries no path to canonicalize.
	if selector == "" || godotid.IsUID(selector) {
		return graph.Node{}, false, nil
	}
	class, recognized := godotid.ClassifyExtension(selector)
	scoped := godotScheme(selector)
	if !recognized && !scoped {
		return graph.Node{}, false, nil
	}
	// The extension is exactly the evidence the identity dropped, so it is what
	// tells a scene apart from the script of the same name. An explicit kind
	// filter is the caller saying it already, and always wins.
	if kind == "" && recognized {
		kind = godotid.NodeKind(class)
	}
	identities, err := s.godotIdentities(ctx, selector, scoped)
	if err != nil {
		return graph.Node{}, false, err
	}
	var matches []graph.Node
	seen := map[string]bool{}
	for _, identity := range identities {
		node, found, err := s.resolveGodotIdentity(ctx, identity, kind)
		if err != nil {
			return graph.Node{}, false, err
		}
		if !found || seen[node.ID] {
			continue
		}
		seen[node.ID] = true
		matches = append(matches, node)
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	if len(matches) == 0 {
		return graph.Node{}, false, nil
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].QualifiedName != matches[j].QualifiedName {
			return matches[i].QualifiedName < matches[j].QualifiedName
		}
		return matches[i].ID < matches[j].ID
	})
	return graph.Node{}, false, &AmbiguousError{Term: selector, Kind: kind,
		Level: graph.MatchQualifiedName, Reason: "by Godot resource path in more than one project",
		Total: len(matches), Candidates: matches}
}

// resolveGodotIdentity resolves one canonical Godot identity to the node that
// declares it. Only qualified-name evidence counts: an identity is the exact
// name such a node carries, so accepting a weaker match would resolve
// "screens/game.tscn" to any node whose name merely contains "screens/game".
func (s *Service) resolveGodotIdentity(ctx context.Context, identity string, kind graph.NodeKind) (graph.Node, bool, error) {
	node, err := s.ResolveKind(ctx, identity, kind)
	if err == nil {
		if node.QualifiedName != identity {
			return graph.Node{}, false, nil
		}
		return node, true, nil
	}
	// An identity shared by two declarations - a scene and its same-named
	// script, when no extension narrowed the kind - is genuinely ambiguous and
	// the caller has to say which. A weaker ambiguity is noise from a selector
	// this identity was never meant to match, and a repository failure is
	// neither: it propagates rather than passing for an absent resource.
	var ambiguous *AmbiguousError
	switch {
	case errors.As(err, &ambiguous):
		if ambiguous.Level == graph.MatchQualifiedName {
			return graph.Node{}, false, err
		}
		return graph.Node{}, false, nil
	case errors.Is(err, ErrNotFound):
		return graph.Node{}, false, nil
	default:
		return graph.Node{}, false, err
	}
}

// godotIdentities canonicalizes a resource-path selector. A path with no scheme
// is already repository-relative. A res:// or user:// path is relative to one
// project, and a selector has no owning file to name that project, so every
// indexed project is a candidate and one that the path escapes contributes
// nothing.
func (s *Service) godotIdentities(ctx context.Context, selector string, scoped bool) ([]string, error) {
	if !scoped {
		if identity := godotid.Resolve("", selector); identity != "" {
			return []string{identity}, nil
		}
		return nil, nil
	}
	directories, err := s.godotProjectDirectories(ctx)
	if err != nil {
		return nil, err
	}
	identities := make([]string, 0, len(directories))
	seen := map[string]bool{}
	for _, directory := range directories {
		identity := godotid.Resolve(directory, selector)
		if identity == "" || seen[identity] {
			continue
		}
		seen[identity] = true
		identities = append(identities, identity)
	}
	return identities, nil
}

// godotProjectLister is the narrow capability a project-relative selector needs:
// enumerating the project.godot files the index holds. It is stated here rather
// than reused from graph.NodeListRepository so a repository that can enumerate
// nodes but cannot name the repositories it federates still resolves one.
type godotProjectLister interface {
	ListNodesByKind(context.Context, graph.NodeListQuery) ([]graph.ScopedNode, error)
}

// godotProjectDirectories lists the directory of every project.godot the index
// holds, which is what a res:// selector is relative to. A repository with no
// indexed project file resolves repository-relatively, the same fallback the
// parser takes when a repository declares no project at all.
func (s *Service) godotProjectDirectories(ctx context.Context) ([]string, error) {
	lister, ok := s.repository.(godotProjectLister)
	if !ok {
		return nil, errors.New("this index cannot enumerate Godot projects, so a project-relative " +
			"selector has nothing to resolve against; name the resource by its repository-relative path")
	}
	nodes, err := lister.ListNodesByKind(ctx, graph.NodeListQuery{Kinds: []graph.NodeKind{graph.KindFile},
		Name: godotid.ProjectFileName, Visibility: graph.LocalNodes, Limit: maxGodotProjects})
	if err != nil {
		return nil, err
	}
	// The bound was reached, so the enumeration proves nothing about what it did
	// not list. It counts files whose path matches the project file name rather
	// than projects, so the refusal says that and does not assert a project
	// count it never established.
	if len(nodes) >= maxGodotProjects {
		return nil, fmt.Errorf("the Godot project enumeration reached its bound of %d files matching %q, "+
			"so no project can be proven to own this reference; name the resource by its "+
			"repository-relative path", maxGodotProjects, godotid.ProjectFileName)
	}
	directories := []string{}
	seen := map[string]bool{}
	for _, scoped := range nodes {
		path := filepath.ToSlash(scoped.Node.QualifiedName)
		// The enumeration matches a name fragment, so a file merely containing
		// "project.godot" in its path is excluded here rather than treated as a
		// project of its own.
		if !strings.EqualFold(pathpkg.Base(path), godotid.ProjectFileName) {
			continue
		}
		directory := godotid.DirOf(path)
		if seen[directory] {
			continue
		}
		seen[directory] = true
		directories = append(directories, directory)
	}
	if len(directories) == 0 {
		return []string{""}, nil
	}
	sort.Strings(directories)
	return directories, nil
}

// godotScheme reports whether a selector carries a Godot reference scheme, which
// makes it project-relative rather than repository-relative.
func godotScheme(selector string) bool {
	for _, prefix := range []string{"res://", "user://"} {
		if strings.HasPrefix(selector, prefix) {
			return true
		}
	}
	return false
}
