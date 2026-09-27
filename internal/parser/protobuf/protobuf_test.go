package protobuf_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	protobufparser "github.com/cafecito-games/grafo/internal/parser/protobuf"
)

func TestParserExtractsProtobufSchema(t *testing.T) {
	source := `syntax = "proto3";
package acme.user.v1;

import public "common/options.proto";

message Profile {}

message User {
  string id = 1;
  repeated Profile profiles = 2;
  map<string, .acme.user.v1.Profile> profiles_by_id = 3;
  oneof identity {
    string email = 4;
    Profile profile = 5;
  }
  optional string nickname = 6;

  message Address {
    string city = 1;
  }
  enum State {
    STATE_UNSPECIFIED = 0;
    STATE_ACTIVE = 1;
  }
}

enum Status {
  STATUS_UNSPECIFIED = 0;
  STATUS_READY = 1;
}

service Users {
  rpc Get(User) returns (stream .acme.user.v1.Profile);
  rpc Upload(stream User) returns (Profile);
}
`
	result := parse(t, "api/user.proto", source)

	file := findNode(t, result.Nodes, graph.KindFile, "api/user.proto")
	if file.Language != "protobuf" || file.Properties["syntax"] != "proto3" || file.Properties["package"] != "acme.user.v1" {
		t.Fatalf("unexpected file metadata: %#v", file)
	}
	assertNode(t, result.Nodes, graph.KindType, "acme.user.v1.Profile", map[string]string{"declaration": "message"})
	assertNode(t, result.Nodes, graph.KindType, "acme.user.v1.User.Address", map[string]string{"declaration": "message"})
	assertNode(t, result.Nodes, graph.KindType, "acme.user.v1.User.State", map[string]string{"declaration": "enum"})
	assertNode(t, result.Nodes, graph.KindType, "acme.user.v1.Status", map[string]string{"declaration": "enum"})
	assertNode(t, result.Nodes, graph.KindInterface, "acme.user.v1.Users", map[string]string{"declaration": "service"})
	assertNode(t, result.Nodes, graph.KindMethod, "acme.user.v1.Users.Get", map[string]string{
		"input": "User", "output": "acme.user.v1.Profile", "server_streaming": "true",
	})

	profiles := assertNode(t, result.Nodes, graph.KindField, "acme.user.v1.User.profiles", map[string]string{
		"type": "Profile", "cardinality": "repeated", "number": "2",
	})
	profilesByID := assertNode(t, result.Nodes, graph.KindField, "acme.user.v1.User.profiles_by_id", map[string]string{
		"type": "map<string, acme.user.v1.Profile>", "cardinality": "map",
		"key_type": "string", "value_type": "acme.user.v1.Profile",
	})
	profile := assertNode(t, result.Nodes, graph.KindField, "acme.user.v1.User.profile", map[string]string{"oneof": "identity"})
	nickname := assertNode(t, result.Nodes, graph.KindField, "acme.user.v1.User.nickname", nil)
	if _, exists := nickname.Properties["oneof"]; exists {
		t.Fatalf("proto3 optional field exposed its synthetic oneof: %#v", nickname.Properties)
	}
	assertNode(t, result.Nodes, graph.KindField, "acme.user.v1.Status.STATUS_READY", map[string]string{
		"declaration": "enum_value", "number": "1",
	})

	assertFact(t, result.Facts, graph.EdgeImports, file.ID, "common/options.proto", map[string]string{"visibility": "public"})
	assertFact(t, result.Facts, graph.EdgeReferences, profiles.ID, "Profile", map[string]string{"role": "field_type"})
	assertFact(t, result.Facts, graph.EdgeReferences, profilesByID.ID, "acme.user.v1.Profile", map[string]string{"role": "map_value"})
	assertFact(t, result.Facts, graph.EdgeReferences, profile.ID, "Profile", map[string]string{"role": "field_type"})

	get := findNode(t, result.Nodes, graph.KindMethod, "acme.user.v1.Users.Get")
	upload := findNode(t, result.Nodes, graph.KindMethod, "acme.user.v1.Users.Upload")
	assertFact(t, result.Facts, graph.EdgeReferences, get.ID, "User", map[string]string{"role": "input"})
	assertFact(t, result.Facts, graph.EdgeReferences, get.ID, "acme.user.v1.Profile", map[string]string{"role": "output", "streaming": "true"})
	assertFact(t, result.Facts, graph.EdgeReferences, upload.ID, "User", map[string]string{"role": "input", "streaming": "true"})
	if len(result.Diagnostics) != 0 {
		t.Fatalf("valid source produced diagnostics: %#v", result.Diagnostics)
	}
}

func TestParserExtractsProto2ExtensionsAndLabels(t *testing.T) {
	source := `syntax = "proto2";
package legacy;

message Record {
  required string name = 1;
  optional group Child = 2 {
    optional int64 id = 1;
  }
}

extend google.protobuf.FieldOptions {
  optional bool sensitive = 50001;
}
`
	result := parse(t, "legacy.proto", source)

	assertNode(t, result.Nodes, graph.KindField, "legacy.Record.name", map[string]string{"cardinality": "required"})
	assertNode(t, result.Nodes, graph.KindType, "legacy.Record.Child", map[string]string{"declaration": "message"})
	extension := assertNode(t, result.Nodes, graph.KindField, "legacy.sensitive", map[string]string{
		"declaration": "extension", "number": "50001", "type": "bool",
	})
	assertFact(t, result.Facts, graph.EdgeReferences, extension.ID, "google.protobuf.FieldOptions", map[string]string{"role": "extendee"})
}

func TestParserReportsMalformedSourceAndKeepsRecoverableDeclarations(t *testing.T) {
	source := `syntax = "proto3";
message Broken {
  string missing_tag = ;
}
message Recovered {
  string value = 1;
}
`
	result := parse(t, "broken.proto", source)
	if len(result.Diagnostics) == 0 {
		t.Fatal("malformed protobuf source produced no diagnostic")
	}
	if result.Diagnostics[0].Line != 3 || !strings.Contains(result.Diagnostics[0].Message, "syntax error") {
		t.Fatalf("unexpected diagnostic: %#v", result.Diagnostics[0])
	}
	assertNode(t, result.Nodes, graph.KindType, "Recovered", map[string]string{"declaration": "message"})
	assertNode(t, result.Nodes, graph.KindField, "Recovered.value", map[string]string{"type": "string"})
}

func TestParserSupportsProtoExtensionCaseInsensitively(t *testing.T) {
	parser := protobufparser.New()
	if !parser.Supports("schema.proto") || !parser.Supports("SCHEMA.PROTO") || parser.Supports("schema.prototxt") {
		t.Fatal("unexpected protobuf extension routing")
	}
}

func TestParserSupportsEditions(t *testing.T) {
	result := parse(t, "edition.proto", `edition = "2023";
message EditionMessage {
  string value = 1;
}
`)
	file := findNode(t, result.Nodes, graph.KindFile, "edition.proto")
	if file.Properties["edition"] != "2023" {
		t.Fatalf("edition metadata = %#v", file.Properties)
	}
	assertNode(t, result.Nodes, graph.KindType, "EditionMessage", map[string]string{"declaration": "message"})
	if len(result.Diagnostics) != 0 {
		t.Fatalf("valid edition source produced diagnostics: %#v", result.Diagnostics)
	}
}

func TestParserHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := protobufparser.New().Parse(ctx, parserapi.Input{Path: "schema.proto", Content: []byte(`syntax = "proto3";`)})
	if err == nil {
		t.Fatal("Parse returned nil error for canceled context")
	}
}

func parse(t *testing.T, path, source string) graph.ParseResult {
	t.Helper()
	result, err := protobufparser.New().Parse(context.Background(), parserapi.Input{
		Path: path, Content: []byte(source), Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string, properties map[string]string) graph.Node {
	t.Helper()
	node := findNode(t, nodes, kind, qualified)
	for key, want := range properties {
		if got := node.Properties[key]; got != want {
			t.Fatalf("node %q property %q = %q, want %q; properties: %#v", qualified, key, got, want, node.Properties)
		}
	}
	if node.Location.Line < 1 || node.Location.Column < 1 || node.Location.EndLine < node.Location.Line {
		t.Fatalf("node %q has invalid location: %#v", qualified, node.Location)
	}
	return node
}

func findNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) graph.Node {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return node
		}
	}
	t.Fatalf("missing %s node %q in %#v", kind, qualified, nodes)
	return graph.Node{}
}

func assertFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, fromID, target string, properties map[string]string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind != kind || fact.FromID != fromID || fact.Target != target {
			continue
		}
		matches := true
		for key, want := range properties {
			if fact.Properties[key] != want {
				matches = false
				break
			}
		}
		if matches {
			return
		}
	}
	t.Fatalf("missing %s fact from %q to %q with %#v in %#v", kind, fromID, target, properties, facts)
}
