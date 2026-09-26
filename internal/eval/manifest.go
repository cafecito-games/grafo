// Package eval runs the deterministic, production-backed resolution corpus.
package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

const ManifestSchemaVersion = 1

type Manifest struct {
	SchemaVersion int              `json:"schema_version"`
	CaseID        string           `json:"case_id"`
	Repositories  []RepositorySpec `json:"repositories"`
	Expect        Expectations     `json:"expect"`
}

type RepositorySpec struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type Expectations struct {
	Nodes          []NodeRef       `json:"nodes"`
	Edges          []EdgeRef       `json:"edges"`
	Queries        []QuerySpec     `json:"queries,omitempty"`
	Ambiguities    []AmbiguitySpec `json:"ambiguities,omitempty"`
	ForbiddenEdges []EdgePattern   `json:"forbidden_edges,omitempty"`
	ForbiddenPaths []PathPattern   `json:"forbidden_paths,omitempty"`
}

type NodeRef struct {
	Repo          string         `json:"repo,omitempty"`
	Kind          graph.NodeKind `json:"kind"`
	QualifiedName string         `json:"qualified_name"`
	External      bool           `json:"external,omitempty"`
}

type SourceRef struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column,omitempty"`
}

type EdgeRef struct {
	From       NodeRef           `json:"from"`
	Relation   graph.EdgeKind    `json:"relation"`
	To         NodeRef           `json:"to"`
	Source     SourceRef         `json:"source"`
	Properties map[string]string `json:"properties,omitempty"`
}

type EdgePattern struct {
	From     NodeRef        `json:"from"`
	Relation graph.EdgeKind `json:"relation"`
	To       NodeRef        `json:"to"`
}

type PathPattern struct {
	From      string           `json:"from"`
	To        string           `json:"to"`
	Direction string           `json:"direction,omitempty"`
	Relations []graph.EdgeKind `json:"relations,omitempty"`
}

type QuerySpec struct {
	ID        string           `json:"id"`
	From      string           `json:"from"`
	To        string           `json:"to"`
	Direction string           `json:"direction,omitempty"`
	Relations []graph.EdgeKind `json:"relations,omitempty"`
	Nodes     []NodeRef        `json:"nodes"`
	Edges     []EdgeRef        `json:"edges"`
}

type AmbiguitySpec struct {
	Selector   string    `json:"selector"`
	Candidates []NodeRef `json:"candidates"`
}

type LoadedManifest struct {
	Path     string
	Manifest Manifest
}

func LoadManifest(path string, input io.Reader) (Manifest, error) {
	content, err := io.ReadAll(input)
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: read: %w", path, err)
	}
	if err := rejectDuplicateKeys(path, content); err != nil {
		return Manifest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, positionJSONError(path, content, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Manifest{}, positionJSONError(path, content, err)
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return manifest, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func rejectDuplicateKeys(path string, content []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := scanJSONValue(path, content, decoder); err != nil {
		return err
	}
	return nil
}

func scanJSONValue(path string, content []byte, decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return positionJSONError(path, content, err)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return positionJSONError(path, content, err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s: object key is not a string", path)
			}
			offset := decoder.InputOffset() - int64(len(mustJSON(key)))
			if seen[key] {
				line, column := lineColumn(content, offset)
				return fmt.Errorf("%s:%d:%d: duplicate key %q", path, line, column, key)
			}
			seen[key] = true
			if err := scanJSONValue(path, content, decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
	case '[':
		for decoder.More() {
			if err := scanJSONValue(path, content, decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
	}
	if err != nil {
		return positionJSONError(path, content, err)
	}
	return nil
}

func mustJSON(value string) []byte {
	encoded, _ := json.Marshal(value)
	return encoded
}

var unknownFieldPattern = regexp.MustCompile(`json: unknown field "([^"]+)"`)

func positionJSONError(path string, content []byte, err error) error {
	if match := unknownFieldPattern.FindStringSubmatch(err.Error()); len(match) == 2 {
		needle := mustJSON(match[1])
		if offset := bytes.Index(content, needle); offset >= 0 {
			line, column := lineColumn(content, int64(offset))
			return fmt.Errorf("%s:%d:%d: unknown field %q", path, line, column, match[1])
		}
	}
	if syntax, ok := err.(*json.SyntaxError); ok {
		line, column := lineColumn(content, syntax.Offset-1)
		return fmt.Errorf("%s:%d:%d: %s", path, line, column, syntax.Error())
	}
	return fmt.Errorf("%s: %w", path, err)
}

func lineColumn(content []byte, offset int64) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(content)) {
		offset = int64(len(content))
	}
	prefix := content[:offset]
	line := bytes.Count(prefix, []byte{'\n'}) + 1
	lastNewline := bytes.LastIndexByte(prefix, '\n')
	return line, len(prefix) - lastNewline
}

func ValidateManifests(manifests []LoadedManifest) error {
	paths := map[string]string{}
	for _, loaded := range manifests {
		if previous, exists := paths[loaded.Manifest.CaseID]; exists {
			return fmt.Errorf("duplicate case_id %q in %s and %s", loaded.Manifest.CaseID, previous, loaded.Path)
		}
		paths[loaded.Manifest.CaseID] = loaded.Path
	}
	return nil
}

func validateManifest(manifest Manifest) error {
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("unsupported schema_version %d (want %d)", manifest.SchemaVersion, ManifestSchemaVersion)
	}
	if !safeIdentifierPattern.MatchString(manifest.CaseID) {
		return fmt.Errorf("case_id %q must use only letters, digits, dot, underscore, or hyphen", manifest.CaseID)
	}
	if len(manifest.Repositories) == 0 {
		return fmt.Errorf("repositories must not be empty")
	}
	repositories := map[string]bool{}
	repositoryPaths := map[string]bool{}
	for _, repository := range manifest.Repositories {
		if repository.ID == "" || repository.Path == "" {
			return fmt.Errorf("repository id and path are required")
		}
		if !safeIdentifierPattern.MatchString(repository.ID) {
			return fmt.Errorf("repository id %q must use only letters, digits, dot, underscore, or hyphen", repository.ID)
		}
		if repositories[repository.ID] {
			return fmt.Errorf("duplicate repository id %q", repository.ID)
		}
		cleanPath := filepath.ToSlash(filepath.Clean(repository.Path))
		if filepath.IsAbs(repository.Path) || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") {
			return fmt.Errorf("repository %q path must stay inside the case", repository.ID)
		}
		if repositoryPaths[cleanPath] {
			return fmt.Errorf("duplicate repository path %q", cleanPath)
		}
		repositories[repository.ID] = true
		repositoryPaths[cleanPath] = true
	}
	for _, node := range manifest.Expect.Nodes {
		if err := validateNodeRef(node, repositories); err != nil {
			return err
		}
	}
	for _, edge := range manifest.Expect.Edges {
		if err := validateEdge(edge.From, edge.Relation, edge.To, repositories); err != nil {
			return err
		}
	}
	for _, edge := range manifest.Expect.ForbiddenEdges {
		if err := validateEdge(edge.From, edge.Relation, edge.To, repositories); err != nil {
			return err
		}
	}
	queryIDs := map[string]bool{}
	for _, query := range manifest.Expect.Queries {
		if query.ID == "" || queryIDs[query.ID] {
			return fmt.Errorf("query ids must be non-empty and unique: %q", query.ID)
		}
		queryIDs[query.ID] = true
		if err := validatePath(query.From, query.To, query.Direction, query.Relations); err != nil {
			return fmt.Errorf("query %q: %w", query.ID, err)
		}
		for _, node := range query.Nodes {
			if err := validateNodeRef(node, repositories); err != nil {
				return fmt.Errorf("query %q: %w", query.ID, err)
			}
		}
		for _, edge := range query.Edges {
			if err := validateEdge(edge.From, edge.Relation, edge.To, repositories); err != nil {
				return fmt.Errorf("query %q: %w", query.ID, err)
			}
		}
	}
	for _, forbidden := range manifest.Expect.ForbiddenPaths {
		if err := validatePath(forbidden.From, forbidden.To, forbidden.Direction, forbidden.Relations); err != nil {
			return fmt.Errorf("forbidden path: %w", err)
		}
	}
	for _, ambiguity := range manifest.Expect.Ambiguities {
		if ambiguity.Selector == "" || len(ambiguity.Candidates) < 2 {
			return fmt.Errorf("ambiguity selector and at least two candidates are required")
		}
		for _, candidate := range ambiguity.Candidates {
			if err := validateNodeRef(candidate, repositories); err != nil {
				return err
			}
		}
	}
	return nil
}

var safeIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validatePath(from, to, direction string, relations []graph.EdgeKind) error {
	if from == "" || to == "" {
		return fmt.Errorf("from and to selectors are required")
	}
	if direction != "" && direction != "outgoing" && direction != "incoming" && direction != "both" {
		return fmt.Errorf("unknown direction %q", direction)
	}
	for _, relation := range relations {
		if !validEdgeKinds[relation] {
			return fmt.Errorf("unknown edge relation %q", relation)
		}
	}
	return nil
}

func validateEdge(from NodeRef, relation graph.EdgeKind, to NodeRef, repositories map[string]bool) error {
	if err := validateNodeRef(from, repositories); err != nil {
		return err
	}
	if !validEdgeKinds[relation] {
		return fmt.Errorf("unknown edge relation %q", relation)
	}
	return validateNodeRef(to, repositories)
}

func validateNodeRef(node NodeRef, repositories map[string]bool) error {
	if !validNodeKinds[node.Kind] {
		return fmt.Errorf("unknown node kind %q", node.Kind)
	}
	if node.QualifiedName == "" {
		return fmt.Errorf("node qualified_name is required")
	}
	if node.Repo != "" && !repositories[node.Repo] {
		return fmt.Errorf("unknown repository %q", node.Repo)
	}
	return nil
}

var validNodeKinds = makeSet([]graph.NodeKind{
	graph.KindRepository, graph.KindFile, graph.KindPackage, graph.KindModule, graph.KindFunction,
	graph.KindMethod, graph.KindType, graph.KindClass, graph.KindInterface, graph.KindField,
	graph.KindVariable, graph.KindParameter, graph.KindTable, graph.KindView, graph.KindColumn,
	graph.KindIndex, graph.KindConfigKey, graph.KindEndpoint, graph.KindEvent, graph.KindExternal,
})

var validEdgeKinds = makeSet([]graph.EdgeKind{
	graph.EdgeContains, graph.EdgeDeclares, graph.EdgeImports, graph.EdgeCalls, graph.EdgeEmbeds,
	graph.EdgeExtends, graph.EdgeImplements, graph.EdgeReadsConfig, graph.EdgeDefines, graph.EdgeExposes,
	graph.EdgeHandledBy, graph.EdgePublishes, graph.EdgeSubscribes, graph.EdgeReferences, graph.EdgeReads,
	graph.EdgeWrites, graph.EdgeHasField, graph.EdgeAssigns, graph.EdgeReturns, graph.EdgePasses,
	graph.EdgeRequests, graph.EdgeDependsOn,
})

func makeSet[T comparable](values []T) map[T]bool {
	result := make(map[T]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func sortNodeRefs(nodes []NodeRef) {
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Repo != nodes[j].Repo {
			return nodes[i].Repo < nodes[j].Repo
		}
		if nodes[i].Kind != nodes[j].Kind {
			return nodes[i].Kind < nodes[j].Kind
		}
		if nodes[i].QualifiedName != nodes[j].QualifiedName {
			return nodes[i].QualifiedName < nodes[j].QualifiedName
		}
		return !nodes[i].External && nodes[j].External
	})
}
