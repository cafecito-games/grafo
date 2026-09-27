package graph_test

import (
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
)

// TestIsDeclarationMemberClassifiesTheWholeVocabulary records the ruling for every
// node kind, so adding a kind to the vocabulary without classifying it fails here
// rather than silently changing how selectors resolve. A kind is a declaration
// member only when every parser builds its qualified name by extending the
// declaring symbol's AND it cannot be referenced on its own bare name; see the
// memberKinds comment in match.go for the reasoning behind each ruling, and for why
// a member ruling alone never decides that a candidate may be suppressed.
func TestIsDeclarationMemberClassifiesTheWholeVocabulary(t *testing.T) {
	members := map[graph.NodeKind]bool{
		// A function's parameters, a type's fields, a table's columns, and local
		// variables. Membership is necessary but not sufficient for suppression:
		// internal/parser/godot overloads KindVariable for hierarchical scene nodes
		// and sub-resources, and KindField for their properties, both of which are
		// declarations GDScript references by name. The structural conditions in
		// query.isSubPartOfCandidate are what keep those safe - see the suppression
		// invariant above memberKinds.
		graph.KindParameter: true,
		graph.KindVariable:  true,
		graph.KindField:     true,
		graph.KindColumn:    true,

		// Containers and declarations, all referenced by their own name.
		graph.KindRepository: false,
		graph.KindFile:       false,
		graph.KindPackage:    false,
		graph.KindModule:     false,
		graph.KindFunction:   false,
		graph.KindMethod:     false,
		graph.KindType:       false,
		graph.KindClass:      false,
		graph.KindInterface:  false,
		graph.KindTable:      false,
		graph.KindView:       false,
		// An index carries its own top-level qualified name, not "table.index".
		graph.KindIndex: false,
		// A config key is resolved from a bare key by config references.
		graph.KindConfigKey: false,
		graph.KindEndpoint:  false,
		// A GDScript signal nests under its class but is connected by bare name.
		graph.KindEvent: false,
		// A markdown section nests under its document but is linked as
		// "path#anchor".
		graph.KindDocSection: false,
		// An unresolved boundary node stands for a declaration elsewhere.
		graph.KindExternal: false,
		// A scene and a saved resource carry their own repository-relative
		// identity; a sub-resource nests under its document but is still a
		// resource in its own right.
		graph.KindGodotScene:    false,
		graph.KindGodotResource: false,
		// A scene node nests under its scene, but GDScript resolves "%Unique"
		// and get_node("Main/Button") against its bare name.
		graph.KindGodotSceneNode: false,
		// An autoload is a project-scoped global resolved by bare name.
		graph.KindGodotAutoload: false,
		// An input action and a node group are project-scoped declarations whose
		// qualified names extend the declaring project.godot path, but every
		// producer names them by their own bare name: Input.is_action_pressed
		// ("jump") and add_to_group("enemies") carry the name and nothing else.
		graph.KindGodotInputAction: false,
		graph.KindGodotNodeGroup:   false,
	}

	for _, kind := range graph.NodeKinds() {
		expected, ruled := members[kind]
		if !ruled {
			t.Fatalf("node kind %q has no declaration-member ruling; classify it in "+
				"internal/graph/match.go (memberKinds) and here", kind)
		}
		if got := graph.IsDeclarationMember(kind); got != expected {
			t.Fatalf("IsDeclarationMember(%q) = %v, want %v", kind, got, expected)
		}
	}
	if len(members) != len(graph.NodeKinds()) {
		t.Fatalf("the ruling table lists %d kinds but the vocabulary has %d",
			len(members), len(graph.NodeKinds()))
	}
	// A kind outside the vocabulary is never a member.
	if graph.IsDeclarationMember("not_a_kind") {
		t.Fatal("an unknown kind must not be treated as a declaration member")
	}
}
