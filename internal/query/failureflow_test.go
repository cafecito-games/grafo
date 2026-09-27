package query_test

import (
	"context"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/query"
)

func TestFailureFlowSeparatesDeclarationsEscapesHandlersAndCleanup(t *testing.T) {
	function := godotNode(graph.KindFunction, "sample.Run", nil)
	errorType := godotNode(graph.KindType, "sample.Problem", nil)
	callee := godotNode(graph.KindFunction, "sample.Load", nil)
	sentinel := godotNode(graph.KindVariable, "sample.ErrMissing", nil)
	cleanup := godotNode(graph.KindFunction, "sample.cleanup", nil)
	recoverNode := godotNode(graph.KindExternal, "builtin.recover", nil)
	recoverNode.External = true
	panicPayload := godotNode(graph.KindExternal, "unresolved:panic@run.go:20", nil)
	panicPayload.External = true
	nodes := map[string]graph.Node{
		function.ID: function, errorType.ID: errorType, callee.ID: callee, sentinel.ID: sentinel,
		cleanup.ID: cleanup, recoverNode.ID: recoverNode, panicPayload.ID: panicPayload,
	}
	repository := &fakeRepository{nodes: nodes, edges: []graph.Edge{
		{ID: "declared", FromID: function.ID, ToID: errorType.ID, Kind: graph.EdgeKind("returns_error"), Properties: map[string]string{"form": "signature"}},
		{ID: "propagates", FromID: function.ID, ToID: callee.ID, Kind: graph.EdgeKind("propagates_error"), Properties: map[string]string{"form": "return_call"}},
		{ID: "handles", FromID: function.ID, ToID: sentinel.ID, Kind: graph.EdgeKind("handles_error"), Properties: map[string]string{"form": "is", "conditional": "true"}},
		{ID: "panics", FromID: function.ID, ToID: panicPayload.ID, Kind: graph.EdgeKind("panics"), Properties: map[string]string{"form": "panic", "unresolved": "true"}},
		{ID: "recovers", FromID: function.ID, ToID: recoverNode.ID, Kind: graph.EdgeKind("recovers"), Properties: map[string]string{"form": "recover"}},
		{ID: "defers", FromID: function.ID, ToID: cleanup.ID, Kind: graph.EdgeKind("defers"), Properties: map[string]string{"form": "defer"}},
	}}

	report, err := query.NewService(repository).FailureFlow(context.Background(), "sample.Run", query.FailureFlowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ErrorReturns) != 1 || report.ErrorReturns[0].Node.QualifiedName != "sample.Problem" {
		t.Fatalf("error returns = %#v", report.ErrorReturns)
	}
	if len(report.Escaping) != 1 || report.Escaping[0].Node.QualifiedName != "sample.Load" {
		t.Fatalf("escaping = %#v", report.Escaping)
	}
	if len(report.Handled) != 1 || !report.Handled[0].Conditional {
		t.Fatalf("handled = %#v", report.Handled)
	}
	if len(report.Panics) != 1 || !report.Panics[0].Unresolved {
		t.Fatalf("panics = %#v", report.Panics)
	}
	if len(report.Recoveries) != 1 || len(report.DeferredCleanup) != 1 {
		t.Fatalf("recovery/cleanup = %#v / %#v", report.Recoveries, report.DeferredCleanup)
	}

	incoming, err := query.NewService(repository).FailureFlow(context.Background(), "sample.Load", query.FailureFlowOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(incoming.Escaping) != 1 || incoming.Escaping[0].Direction != query.Incoming ||
		incoming.Escaping[0].Node.QualifiedName != "sample.Run" {
		t.Fatalf("incoming escaping = %#v", incoming.Escaping)
	}
}
