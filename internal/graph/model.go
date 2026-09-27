package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const SchemaVersion = 3

type NodeKind string

const (
	KindRepository NodeKind = "repository"
	KindFile       NodeKind = "file"
	KindPackage    NodeKind = "package"
	KindModule     NodeKind = "module"
	KindFunction   NodeKind = "function"
	KindMethod     NodeKind = "method"
	KindType       NodeKind = "type"
	KindClass      NodeKind = "class"
	KindInterface  NodeKind = "interface"
	KindField      NodeKind = "field"
	KindVariable   NodeKind = "variable"
	KindParameter  NodeKind = "parameter"
	KindTable      NodeKind = "table"
	KindView       NodeKind = "view"
	KindColumn     NodeKind = "column"
	KindIndex      NodeKind = "index"
	KindConfigKey  NodeKind = "config_key"
	KindEndpoint   NodeKind = "endpoint"
	KindEvent      NodeKind = "event"
	KindDocSection NodeKind = "document_section"
	KindExternal   NodeKind = "external"
)

type EdgeKind string

const (
	EdgeContains    EdgeKind = "contains"
	EdgeDeclares    EdgeKind = "declares"
	EdgeImports     EdgeKind = "imports"
	EdgeExports     EdgeKind = "exports"
	EdgeCalls       EdgeKind = "calls"
	EdgeEmbeds      EdgeKind = "embeds"
	EdgeExtends     EdgeKind = "extends"
	EdgeImplements  EdgeKind = "implements"
	EdgeReadsConfig EdgeKind = "reads_config"
	EdgeDefines     EdgeKind = "defines"
	EdgeExposes     EdgeKind = "exposes"
	EdgeHandledBy   EdgeKind = "handled_by"
	EdgePublishes   EdgeKind = "publishes"
	EdgeSubscribes  EdgeKind = "subscribes"
	EdgeReferences  EdgeKind = "references"
	EdgeReads       EdgeKind = "reads"
	EdgeWrites      EdgeKind = "writes"
	EdgeHasField    EdgeKind = "has_field"
	EdgeAssigns     EdgeKind = "assigns"
	EdgeReturns     EdgeKind = "returns"
	EdgePasses      EdgeKind = "passes"
	EdgeRequests    EdgeKind = "requests"
	EdgeDependsOn   EdgeKind = "depends_on"
	EdgeDocuments   EdgeKind = "documents"
)

type Location struct {
	Path    string `json:"path,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	EndLine int    `json:"end_line,omitempty"`
}

type Node struct {
	ID            string            `json:"id"`
	Kind          NodeKind          `json:"kind"`
	Name          string            `json:"name"`
	QualifiedName string            `json:"qualified_name"`
	Language      string            `json:"language,omitempty"`
	Location      Location          `json:"location,omitempty"`
	Properties    map[string]string `json:"properties,omitempty"`
	OwnerFile     string            `json:"-"`
	External      bool              `json:"external,omitempty"`
}

type Fact struct {
	ID         string            `json:"id"`
	FromID     string            `json:"from_id"`
	Kind       EdgeKind          `json:"kind"`
	TargetID   string            `json:"target_id,omitempty"`
	Target     string            `json:"target,omitempty"`
	TargetKind NodeKind          `json:"target_kind,omitempty"`
	Location   Location          `json:"location,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
	OwnerFile  string            `json:"-"`
}

type Edge struct {
	ID         string            `json:"id"`
	FromID     string            `json:"from_id"`
	ToID       string            `json:"to_id"`
	Kind       EdgeKind          `json:"kind"`
	Location   Location          `json:"location,omitempty"`
	Properties map[string]string `json:"properties,omitempty"`
	FactID     string            `json:"fact_id,omitempty"`
}

type Diagnostic struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type ParseResult struct {
	Nodes       []Node
	Facts       []Fact
	Diagnostics []Diagnostic
}

func StableID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return prefix + ":" + hex.EncodeToString(h.Sum(nil))[:20]
}

func NodeID(kind NodeKind, qualifiedName string, discriminator ...string) string {
	parts := []string{string(kind), qualifiedName}
	parts = append(parts, discriminator...)
	return StableID("n", parts...)
}

func FactID(owner, from string, kind EdgeKind, target string, line, ordinal int) string {
	return StableID("f", owner, from, string(kind), target, fmt.Sprint(line), fmt.Sprint(ordinal))
}

func EdgeID(factID, toID string) string { return StableID("e", factID, toID) }

func MarshalProperties(properties map[string]string) string {
	if len(properties) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(properties)
	return string(b)
}

func UnmarshalProperties(raw string) map[string]string {
	if raw == "" || raw == "{}" {
		return nil
	}
	var result map[string]string
	if json.Unmarshal([]byte(raw), &result) != nil {
		return map[string]string{"_invalid": raw}
	}
	return result
}

func SortedPropertyKeys(properties map[string]string) []string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func SimpleName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, ".:/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// DataResourceKinds lists the node kinds that represent stored data resources.
// Extending this list extends every data catalog without changing query or
// presentation code.
func DataResourceKinds() []NodeKind { return []NodeKind{KindTable, KindView} }

// IsDataResourceKind reports whether kind names a stored data resource.
func IsDataResourceKind(kind NodeKind) bool {
	for _, candidate := range DataResourceKinds() {
		if candidate == kind {
			return true
		}
	}
	return false
}
