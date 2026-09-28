package eval

import (
	"strings"
	"testing"
)

func TestLoadManifestRejectsUnknownFieldsBeforeIndexing(t *testing.T) {
	_, err := LoadManifest("case/manifest.json", strings.NewReader(`{
  "schema_version": 2,
  "case_id": "sample",
  "repositories": [{"id": "app", "path": "repos/app"}],
  "expect": {"nodes": [], "edges": []},
  "surprise": true
}`))
	if err == nil || !strings.Contains(err.Error(), `case/manifest.json:6:3: unknown field "surprise"`) {
		t.Fatalf("expected positioned unknown-field error, got %v", err)
	}
}

func TestLoadManifestRejectsDuplicateKeysBeforeIndexing(t *testing.T) {
	_, err := LoadManifest("case/manifest.json", strings.NewReader(`{
  "schema_version": 2,
  "case_id": "first",
  "case_id": "second",
  "repositories": [{"id": "app", "path": "repos/app"}],
  "expect": {"nodes": [], "edges": []}
}`))
	if err == nil || !strings.Contains(err.Error(), `case/manifest.json:4:3: duplicate key "case_id"`) {
		t.Fatalf("expected positioned duplicate-key error, got %v", err)
	}
}

func TestLoadManifestValidatesSchemaKindsAndRelations(t *testing.T) {
	tests := []struct {
		name, replace, want string
	}{
		{name: "schema", replace: `"schema_version": 3`, want: "unsupported schema_version 3"},
		{name: "kind", replace: `"kind": "mystery"`, want: `unknown node kind "mystery"`},
		{name: "relation", replace: `"relation": "guesses"`, want: `unknown edge relation "guesses"`},
	}
	base := `{
  "schema_version": 2,
  "case_id": "sample",
  "repositories": [{"id": "app", "path": "repos/app"}],
  "expect": {
    "nodes": [{"repo": "app", "kind": "function", "qualified_name": "run"}],
    "edges": [{"from": {"repo": "app", "kind": "function", "qualified_name": "run"}, "relation": "calls", "to": {"kind": "external", "qualified_name": "missing", "external": true}, "source": {"path": "main.go", "line": 1}}]
  }
}`
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			switch test.name {
			case "schema":
				input = strings.Replace(input, `"schema_version": 2`, test.replace, 1)
			case "kind":
				input = strings.Replace(input, `"kind": "function"`, test.replace, 1)
			case "relation":
				input = strings.Replace(input, `"relation": "calls"`, test.replace, 1)
			}
			_, err := LoadManifest("manifest.json", strings.NewReader(input))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}

func TestValidateManifestsRejectsDuplicateCaseIDs(t *testing.T) {
	manifest := Manifest{SchemaVersion: 2, CaseID: "duplicate", Repositories: []RepositorySpec{{ID: "app", Path: "repos/app"}}}
	err := ValidateManifests([]LoadedManifest{{Path: "a/manifest.json", Manifest: manifest}, {Path: "b/manifest.json", Manifest: manifest}})
	if err == nil || !strings.Contains(err.Error(), `duplicate case_id "duplicate"`) || !strings.Contains(err.Error(), "a/manifest.json") || !strings.Contains(err.Error(), "b/manifest.json") {
		t.Fatalf("expected duplicate case error with both paths, got %v", err)
	}
}

func TestLoadManifestRejectsUnsafeAndDuplicateRepositories(t *testing.T) {
	tests := []struct {
		name, repositories, want string
	}{
		{name: "unsafe id", repositories: `[{"id":"../app","path":"repos/app"}]`, want: `repository id "../app"`},
		{name: "escaping path", repositories: `[{"id":"app","path":"../app"}]`, want: "path must stay inside the case"},
		{name: "duplicate path", repositories: `[{"id":"one","path":"repos/app"},{"id":"two","path":"repos/app"}]`, want: `duplicate repository path "repos/app"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := `{"schema_version":2,"case_id":"sample","repositories":` + test.repositories + `,"expect":{"nodes":[],"edges":[]}}`
			_, err := LoadManifest("manifest.json", strings.NewReader(input))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q, got %v", test.want, err)
			}
		})
	}
}

func TestSnapshotsTreatNullAndEmptyCollectionsEqually(t *testing.T) {
	manifest, err := LoadManifest("manifest.json", strings.NewReader(`{
  "schema_version": 2,
  "case_id": "queryless",
  "repositories": [{"id": "app", "path": "repos/app"}],
  "expect": {"nodes": null, "edges": null}
}`))
	if err != nil {
		t.Fatal(err)
	}
	expected := Snapshot{Nodes: manifest.Expect.Nodes, Edges: manifest.Expect.Edges, Queries: manifest.Expect.Queries}
	actual := Snapshot{Nodes: []NodeRef{}, Edges: []EdgeRef{}, Queries: []QuerySpec{}}
	if err := compareSnapshots("queryless", expected, actual); err != nil {
		t.Fatal(err)
	}
}
