package main

import (
	"github.com/cafecito-games/grafo/internal/graph"
	"reflect"
	"testing"
)

func TestBehaviorTermsPreserveIdentifierWords(t *testing.T) {
	got := terms("Where is PlayerRespawn handled?")
	if !reflect.DeepEqual(got, []string{"player", "respawn"}) {
		t.Fatal(got)
	}
}

func TestMultiTermDeclarationOutranksIncidentalSubstring(t *testing.T) {
	ts := terms("send friend request")
	relevant := rank(graph.Node{Name: "SendFriendRequest", QualifiedName: "social.Service.SendFriendRequest"}, ts)
	incidental := rank(graph.Node{Name: "request", QualifiedName: "http.request"}, ts)
	if relevant.Score <= incidental.Score || len(relevant.Matched) != 3 {
		t.Fatalf("%+v vs %+v", relevant, incidental)
	}
}

func TestNoMatchDoesNotAcquireRelevance(t *testing.T) {
	got := rank(graph.Node{Name: "Update", QualifiedName: "world.Update"}, terms("quantum banana"))
	if got.Score != 0 || len(got.Matched) != 0 {
		t.Fatal(got)
	}
}
