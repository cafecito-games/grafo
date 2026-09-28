// Package transport defines the language-neutral transport extraction contract.
// Language adapters match exact API identities and normalize their evidence;
// this package owns stable operation nodes and graph emission.
package transport

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
)

type Direction string

const (
	Send    Direction = "send"
	Receive Direction = "receive"
)

// Spec describes one exact transport API. Argument positions are zero-based;
// a negative position means that the API does not expose that value directly.
type Spec struct {
	Protocol            string
	API                 string
	Direction           Direction
	ChannelPosition     int
	PayloadPosition     int
	ReliabilityPosition int
}

// Adapter recognizes exact API identities. Implementations must not fall back
// to method-name matching.
type Adapter interface {
	Match(identity string) (Spec, bool)
}

// ExactAdapter is the common adapter implementation used by language-specific
// extractors after they have established a typed/static API identity.
type ExactAdapter map[string]Spec

func (a ExactAdapter) Match(identity string) (Spec, bool) {
	spec, ok := a[identity]
	return spec, ok
}

// Operation is normalized evidence for one callsite. Carries is emitted only
// when MessageID and Message are both present and PayloadStatus is proven.
type Operation struct {
	Spec
	Channel       string
	ChannelStatus string
	Reliability   string
	PayloadStatus string
	Proof         string
	WrapperDepth  int
	Location      graph.Location
	MessageID     string
	Message       string
	Binding       string
}

// Emit declares one stable callsite operation and its language-neutral edges.
func Emit(b *parserapi.Builder, sourceID string, operation Operation) string {
	properties := map[string]string{
		"protocol":             operation.Protocol,
		"api":                  operation.API,
		"direction":            string(operation.Direction),
		"channel_status":       status(operation.ChannelStatus),
		"reliability":          status(operation.Reliability),
		"payload_status":       status(operation.PayloadStatus),
		"payload_position":     strconv.Itoa(operation.PayloadPosition),
		"channel_position":     strconv.Itoa(operation.ChannelPosition),
		"reliability_position": strconv.Itoa(operation.ReliabilityPosition),
		"proof":                status(operation.Proof),
	}
	if operation.Channel != "" {
		properties["channel"] = operation.Channel
	}
	if operation.WrapperDepth > 0 {
		properties["wrapper_depth"] = strconv.Itoa(operation.WrapperDepth)
	}
	qualified := QualifiedName(operation)
	name := string(operation.Direction) + " " + graph.SimpleName(operation.API)
	operationID := b.AddNode(graph.Node{Kind: graph.KindTransportOperation, Name: name,
		QualifiedName: qualified, Location: operation.Location, Properties: properties})
	relation := graph.EdgeSends
	if operation.Direction == Receive {
		relation = graph.EdgeReceives
	}
	b.AddFact(sourceID, relation, operationID, "", graph.KindTransportOperation,
		operation.Location, map[string]string{"protocol": operation.Protocol, "proof": status(operation.Proof)})
	if operation.PayloadStatus == "proven" && operation.MessageID != "" && operation.Message != "" {
		carry := map[string]string{
			"protocol": operation.Protocol, "proof": status(operation.Proof),
			"payload_position": strconv.Itoa(operation.PayloadPosition),
		}
		if operation.Binding != "" {
			carry["binding"] = operation.Binding
		}
		if operation.WrapperDepth > 0 {
			carry["wrapper_depth"] = strconv.Itoa(operation.WrapperDepth)
		}
		b.AddFact(operationID, graph.EdgeCarries, operation.MessageID, operation.Message,
			graph.KindType, operation.Location, carry)
	}
	return operationID
}

// QualifiedName returns the stable callsite identity shared by emitters that
// need to attach later evidence (for example, a downstream decode) before the
// operation node itself is walked.
func QualifiedName(operation Operation) string {
	path := filepath.ToSlash(filepath.Clean(operation.Location.Path))
	return fmt.Sprintf("%s:%d:%d:%s:%s", path, operation.Location.Line,
		operation.Location.Column, operation.API, operation.Direction)
}

func OperationID(b *parserapi.Builder, operation Operation) string {
	return graph.NodeID(graph.KindTransportOperation, QualifiedName(operation), b.Input.RepoID, operation.Location.Path)
}

func status(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}
