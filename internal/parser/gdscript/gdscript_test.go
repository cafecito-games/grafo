package gdscript_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	parserapi "github.com/cafecito-games/grafo/internal/parser"
	gdscriptparser "github.com/cafecito-games/grafo/internal/parser/gdscript"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestParserClassifiesOnlyStructurallyBackedGodotTests(t *testing.T) {
	content := []byte(`extends GutTest

func test_damage() -> void:
	assert_true(true)

func before_each() -> void:
	pass

func helper() -> void:
	pass
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "tests/player_test.gd", Content: content, Repository: "sample", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	testNode := gdNodeNamed(t, result.Nodes, "test_damage")
	if testNode.Kind != graph.KindTest || testNode.Properties["test_subtype"] != "test" ||
		testNode.Properties["test_framework"] != "gdscript" || testNode.Properties["test_base"] != "GutTest" {
		t.Fatalf("GUT test metadata = %#v", testNode)
	}
	lifecycle := gdNodeNamed(t, result.Nodes, "before_each")
	if lifecycle.Kind != graph.KindMethod || lifecycle.Properties["test_role"] != "lifecycle" ||
		lifecycle.Properties["test_lifecycle"] != "before_each" {
		t.Fatalf("lifecycle metadata = %#v", lifecycle)
	}
	helper := gdNodeNamed(t, result.Nodes, "helper")
	if helper.Kind != graph.KindMethod || helper.Properties["test_role"] != "helper" {
		t.Fatalf("test helper metadata = %#v", helper)
	}

	plain, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/player.gd", Content: []byte("extends Node\nfunc test_damage():\n\tpass\n"),
		Repository: "sample", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if node := gdNodeNamed(t, plain.Nodes, "test_damage"); node.Kind != graph.KindMethod || node.Properties["test_role"] != "" {
		t.Fatalf("unbacked test-like method = %#v", node)
	}
}

func TestParserClassifiesTestsThroughLaterSameFileBaseDeclarations(t *testing.T) {
	content := []byte(`extends RootSpec

func test_root_late_base() -> void:
	pass

class DerivedSpec extends InnerSpec:
	func test_inner_late_base() -> void:
		pass

class RootSpec extends GutTest:
	pass

class InnerSpec extends GutTest:
	pass
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "tests/late_base_test.gd", Content: content, Repository: "sample", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"test_root_late_base", "test_inner_late_base"} {
		node := gdNodeNamed(t, result.Nodes, name)
		if node.Kind != graph.KindTest || node.Properties["test_base"] == "" {
			t.Fatalf("later-declared base did not classify %s: %#v", name, node)
		}
	}
}

func TestConfiguredGodotTestBasesAreValidatedAndSemantic(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "grafo.yaml", "tests:\n  gdscript_bases: [SpecBase]\n")
	input := parserapi.Input{Root: root, Path: "tests/spec.gd", Content: []byte("extends SpecBase\nfunc test_custom():\n\tpass\n"),
		Repository: "sample", RepoID: "repo"}
	parser := gdscriptparser.New()
	configured, err := parser.Parse(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if node := gdNodeNamed(t, configured.Nodes, "test_custom"); node.Kind != graph.KindTest || node.Properties["test_base"] != "SpecBase" {
		t.Fatalf("configured test base was not honored: %#v", node)
	}
	firstKey, err := parser.SemanticKey(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "grafo.yaml", "tests:\n  gdscript_bases: [OtherBase]\n")
	secondKey, err := parser.SemanticKey(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if firstKey == secondKey {
		t.Fatal("test-base configuration did not invalidate the GDScript semantic key")
	}

	writeFile(t, root, "grafo.yaml", "tests:\n  gdscript_bases: [42]\n")
	invalid, err := parser.Parse(context.Background(), input)
	if err != nil {
		t.Fatalf("invalid test config must fall back to built-ins: %v", err)
	}
	if node := gdNodeNamed(t, invalid.Nodes, "test_custom"); node.Kind == graph.KindTest {
		t.Fatalf("invalid config enabled custom base: %#v", node)
	}
	foundWarning := false
	for _, diagnostic := range invalid.Diagnostics {
		foundWarning = foundWarning || diagnostic.Level == "warning" && strings.Contains(diagnostic.Message, "gdscript_bases")
	}
	if !foundWarning {
		t.Fatalf("invalid test config was not diagnosed: %#v", invalid.Diagnostics)
	}
}

func gdNodeNamed(t *testing.T, nodes []graph.Node, name string) graph.Node {
	t.Helper()
	for _, node := range nodes {
		if node.Name == name {
			return node
		}
	}
	t.Fatalf("node %q not found: %#v", name, nodes)
	return graph.Node{}
}

func TestParserExtractsGodotSymbolsAndWiring(t *testing.T) {
	content := []byte(`class_name Player extends CharacterBody2D

signal health_changed(value: int)
const Enemy = preload('res://actors/enemy.gd')
const EnemyUID = preload('uid://enemy123')
@export var speed: float = 10.0
var health: int = 100

func take_damage(amount: int) -> int:
	var applied: int = amount
	var enemy: Enemy = Enemy.new()
	health = applied
	health_changed.emit(health)
	health_changed.connect(on_health_changed)
	enemy.attack(applied)
	var mode = OS.get_environment("GAME_MODE")
	return applied
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "actors/player.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasNode(t, result.Nodes, graph.KindClass, "Player")
	assertHasNode(t, result.Nodes, graph.KindMethod, "take_damage")
	assertHasNode(t, result.Nodes, graph.KindParameter, "amount")
	assertHasNode(t, result.Nodes, graph.KindField, "speed")
	assertHasNode(t, result.Nodes, graph.KindField, "health")
	assertHasNode(t, result.Nodes, graph.KindVariable, "applied")
	assertHasNode(t, result.Nodes, graph.KindEvent, "health_changed")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "CharacterBody2D")
	assertHasFact(t, result.Facts, graph.EdgeImports, "actors/enemy")
	assertHasFact(t, result.Facts, graph.EdgeImports, "uid://enemy123")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "actors/enemy.attack")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "GAME_MODE")
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgePasses)
	assertHasFactKind(t, result.Facts, graph.EdgeReturns)
	assertHasFactKind(t, result.Facts, graph.EdgePublishes)
	assertHasFactKind(t, result.Facts, graph.EdgeSubscribes)
}

func TestParserExtractsTypedAndConfiguredHTTPRequests(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "grafo.yaml", `http:
  request_apis:
    - language: gdscript
      symbol: AuthAPI.request_json
      method_argument: 0
      url_argument: 1
    - language: gdscript
      symbol: HTTPRequest.request
      method_argument: 2
      route_argument: 0
`)
	content := []byte(`class_name Client

const API_ROOT = "https://api.example.test"
const USERS = "/users/"

func send(auth: AuthAPI, request: HTTPRequest, other, dynamic_path: String) -> void:
	var user_id = "42"
	var route = API_ROOT + USERS
	route += "%s" % user_id
	route += "?expand=true"
	request.request(route, [], HTTPClient.METHOD_GET)
	var inferred = HTTPRequest.new()
	inferred.request("/orders", [], HTTPClient.METHOD_POST, "{}")
	request.request("/default")
	auth.request_json(HTTPClient.METHOD_PUT, "/profiles/%s?view=full" % "me")
	other.request("/forbidden-untyped", [], HTTPClient.METHOD_GET)
	request.request("/forbidden-numeric", [], 2)
	request.request(42, [], HTTPClient.METHOD_GET)
	request.request(dynamic_path, [], HTTPClient.METHOD_GET)
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := map[string][]graph.Fact{}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeRequests {
			requests[fact.Target] = append(requests[fact.Target], fact)
		}
	}
	for _, target := range []string{
		"GET https://api.example.test/users/42",
		"POST /orders",
		"GET /default",
		"PUT /profiles/me",
	} {
		if len(requests[target]) != 1 {
			t.Fatalf("request %q count = %d; facts = %#v", target, len(requests[target]), result.Facts)
		}
	}
	get := requests["GET https://api.example.test/users/42"][0]
	if get.Properties["http_method"] != "GET" || get.Properties["http_route"] != "/users/42" ||
		get.Properties["http_query"] != "expand=true" || get.Properties["http_authority"] != "api.example.test" ||
		get.Properties["http_signature"] != "builtin,configured" || get.Properties["http_api"] != "HTTPRequest.request" ||
		get.Properties["http_route_expression"] != "route" || get.Properties["http_method_expression"] != "HTTPClient.METHOD_GET" {
		t.Fatalf("typed request evidence = %#v", get.Properties)
	}
	configured := requests["PUT /profiles/me"][0]
	if configured.Properties["http_signature"] != "configured" || configured.Properties["http_api"] != "AuthAPI.request_json" ||
		configured.Properties["http_config"] != "grafo.yaml:3" || configured.Properties["http_raw_route"] != "/profiles/me?view=full" {
		t.Fatalf("configured request evidence = %#v", configured.Properties)
	}
	for target := range requests {
		if strings.Contains(target, "forbidden") {
			t.Fatalf("unsupported HTTP call emitted request %q: %#v", target, requests[target])
		}
	}
}

func TestLegacyAndAdapterHTTPConfigurationShareOneProjection(t *testing.T) {
	content := []byte("extends Node\nfunc send(auth: AuthAPI):\n\tauth.fetch(HTTPClient.METHOD_PATCH, \"/profiles/me?full=true\")\n")
	parse := func(configuration string) graph.Fact {
		t.Helper()
		root := testtemp.Dir(t)
		writeFile(t, root, "grafo.yaml", configuration)
		result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
			Root: root, Path: "client.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
		})
		if err != nil {
			t.Fatal(err)
		}
		return findFactWithTarget(t, result.Facts, graph.EdgeRequests, "PATCH /profiles/me")
	}
	legacy := parse("http:\n  request_apis:\n    - language: gdscript\n      symbol: AuthAPI.fetch\n      method_argument: 0\n      url_argument: 1\n")
	adapter := parse("adapters:\n  - match: {language: gdscript, symbol: AuthAPI.fetch}\n    effects:\n      - kind: http.request\n        roles: {method: {argument: 0}, url: {argument: 1}}\n")
	for _, key := range []string{"http_api", "http_method", "http_raw_method", "http_route", "http_raw_route", "http_query", "http_signature", "http_source"} {
		if legacy.Properties[key] != adapter.Properties[key] {
			t.Fatalf("legacy/new HTTP %s differ: legacy=%#v adapter=%#v", key, legacy.Properties, adapter.Properties)
		}
	}
	if legacy.Properties["adapter_symbol"] != "AuthAPI.fetch" || adapter.Properties["adapter_symbol"] != "AuthAPI.fetch" {
		t.Fatalf("legacy/new adapter provenance missing: legacy=%#v adapter=%#v", legacy.Properties, adapter.Properties)
	}
}

func TestParserMapsOnlySymbolicGodotHTTPMethods(t *testing.T) {
	content := []byte(`extends Node

const DEFAULT_METHOD = HTTPClient.METHOD_HEAD

func send(request: HTTPRequest) -> void:
	request.request("/get", [], HTTPClient.METHOD_GET)
	request.request("/head", [], HTTPClient.METHOD_HEAD)
	request.request("/post", [], HTTPClient.METHOD_POST)
	request.request("/put", [], HTTPClient.METHOD_PUT)
	request.request("/patch", [], HTTPClient.METHOD_PATCH)
	request.request("/delete", [], HTTPClient.METHOD_DELETE)
	request.request("/options", [], HTTPClient.METHOD_OPTIONS)
	request.request("/connect", [], HTTPClient.METHOD_CONNECT)
	request.request("/trace", [], HTTPClient.METHOD_TRACE)
	request.request("/constant", [], DEFAULT_METHOD)
	request.request("/numeric", [], 0)
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "client.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"GET /get": true, "HEAD /head": true, "POST /post": true, "PUT /put": true,
		"PATCH /patch": true, "DELETE /delete": true, "OPTIONS /options": true, "CONNECT /connect": true, "TRACE /trace": true,
		"HEAD /constant": true}
	for _, fact := range result.Facts {
		if fact.Kind != graph.EdgeRequests {
			continue
		}
		if !want[fact.Target] {
			t.Fatalf("unexpected request fact %#v", fact)
		}
		delete(want, fact.Target)
	}
	if len(want) != 0 {
		t.Fatalf("missing method mappings: %#v", want)
	}
}

func TestParserDoesNotTreatLocallyDeclaredRequestAsGodotHTTP(t *testing.T) {
	content := []byte(`class_name HTTPRequest

func request(_url: String, _headers: Array, _method: int) -> void:
	pass

func send() -> void:
	request("/not-http", [], HTTPClient.METHOD_GET)
	self.request("/also-not-http", [], HTTPClient.METHOD_POST)
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "lookalike.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeRequests {
			t.Fatalf("locally declared request emitted HTTP evidence: %#v", fact)
		}
	}
}

func TestParserExtractsHTTPRequestFromTypedSelfField(t *testing.T) {
	content := []byte(`class_name Client

var http: HTTPRequest

func send() -> void:
	self.http.request("/field", [], HTTPClient.METHOD_GET)
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "client.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeRequests, "GET /field")
}

func TestParserDiagnosesRejectedConfiguredIdentityAndUnsupportedKnownFormat(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "grafo.yaml", `http:
  request_apis:
    - language: gdscript
      symbol: AuthAPI.request_json
      method_argument: 0
      url_argument: 1
`)
	content := []byte(`extends Node

func send() -> void:
	var AuthAPI = get_node("API")
	AuthAPI.request_json(HTTPClient.METHOD_GET, "/shadowed")
	AuthAPI.request_json(HTTPClient.METHOD_GET, "/shadowed-again")
	var http = HTTPRequest.new()
	http.request("/precision/%.3f" % 1.25, [], HTTPClient.METHOD_GET)
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	identityWarnings, formatWarnings := 0, 0
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Message, "configured HTTP request API") && strings.Contains(diagnostic.Message, "AuthAPI.request_json") {
			identityWarnings++
		}
		if strings.Contains(diagnostic.Message, "unsupported GDScript HTTP route percent formatting") {
			formatWarnings++
		}
	}
	if identityWarnings != 1 || formatWarnings != 1 {
		t.Fatalf("diagnostics = %#v, want one identity and one format warning", result.Diagnostics)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeRequests {
			t.Fatalf("rejected identity/format emitted request edge: %#v", fact)
		}
	}
}

func TestAdapterSemanticKeyAndDependencyTrackOnlyAdapterConfiguration(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "grafo.yaml", "components:\n  - name: client\n    roots: [client]\n")
	parser := gdscriptparser.New()
	input := parserapi.Input{Root: root, Path: "client/main.gd", Content: []byte("extends Node\n")}
	first, err := parser.SemanticKey(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "grafo.yaml", "components:\n  - name: other\n    roots: [other]\nadapters:\n  - match: {language: gdscript, symbol: AuthAPI.send}\n    effects:\n      - kind: http.request\n        roles: {method: {argument: 0}, url: {argument: 1}}\n")
	second, err := parser.SemanticKey(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("adapter config edit did not change semantic key: %q", first)
	}
	writeFile(t, root, "grafo.yaml", "components:\n  - name: third\n    roots: [third]\nadapters:\n  - match: {language: gdscript, symbol: AuthAPI.send}\n    effects:\n      - kind: http.request\n        roles: {url: {argument: 1}, method: {argument: 0}}\n")
	third, err := parser.SemanticKey(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second != third {
		t.Fatalf("unrelated configuration or role order changed semantic key: %q != %q", second, third)
	}
	dependencies := parser.SemanticDependencies()
	if len(dependencies) != 1 || dependencies[0] != "grafo.yaml" {
		t.Fatalf("semantic dependencies = %#v", dependencies)
	}
}

func TestParserExtractsENetTransportEvidence(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, ".gitignore", "generated/\n")
	writeFile(t, root, "buf.yaml", "version: v2\nmodules:\n  - path: proto\n")
	writeFile(t, root, "buf.gen.yaml", `version: v2
plugins:
  - local: protoc-gen-gdscript
    out: generated
`)
	writeFile(t, root, "proto/envelope.proto", `syntax = "proto3";
package acme.v1;
message Envelope { string text = 1; }
`)
	content := []byte(`class_name TransportClient

const GAMEPLAY_CHANNEL = 3

class Lookalike:
	func send(_channel: int, _payload: PackedByteArray, _flags: int) -> void:
		pass

var stored_message: AcmeV1EnvelopeEnvelope

func send_message(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	peer.send(GAMEPLAY_CHANNEL, message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)

func relay(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	send_message(peer, message)

func conflicting(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope, unknown: PackedByteArray) -> void:
	peer.send(1, message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)
	peer.send(2, unknown, ENetPacketPeer.FLAG_UNSEQUENCED)

func deep1(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	send_message(peer, message)
func deep2(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	deep1(peer, message)
func deep3(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	deep2(peer, message)
func deep4(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	deep3(peer, message)
func deep5(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	deep4(peer, message)
func deep6(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	deep5(peer, message)
func deep7(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	deep6(peer, message)
func deep8(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	deep7(peer, message)
func deep9(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope) -> void:
	deep8(peer, message)

func use_transport(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope, channel: int, flags: int, unknown: PackedByteArray) -> void:
	relay(peer, message)
	conflicting(peer, message, unknown)
	deep9(peer, message)
	peer.send(channel, unknown, flags)
	var packet = peer.get_packet()
	var decoded = AcmeV1EnvelopeEnvelope.from_bytes(packet)
	decoded.set_text("received")

func ordinary(value: Lookalike, payload: PackedByteArray) -> void:
	value.send(3, payload, ENetPacketPeer.FLAG_RELIABLE)

func connection_transport(connection: ENetConnection, message: AcmeV1EnvelopeEnvelope) -> void:
	connection.broadcast(4, message.to_bytes(), 0)
	connection.service()

func uncertain_payload(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope, unknown: PackedByteArray, cond: bool) -> void:
	var payload = message.to_bytes()
	if cond:
		payload = unknown
	peer.send(3, payload, ENetPacketPeer.FLAG_RELIABLE)

func agreed_payload(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope, cond: bool) -> void:
	var payload
	if cond:
		payload = message.to_bytes()
	else:
		payload = message.to_bytes()
	peer.send(3, payload, ENetPacketPeer.FLAG_RELIABLE)

func default_wrapper(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope = null) -> void:
	peer.send(5, message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)

func omitted_default(peer: ENetPacketPeer, unrelated: AcmeV1EnvelopeEnvelope) -> void:
	default_wrapper(peer)

func shadowed_evidence(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope, items: Array) -> void:
	if items.is_empty():
		var payload = message.to_bytes()
		var packet = peer.get_packet()
	for payload in items:
		peer.send(6, payload, ENetPacketPeer.FLAG_RELIABLE)
	var callback = func(packet):
		AcmeV1EnvelopeEnvelope.from_bytes(packet)
	callback.call(PackedByteArray())

func shadowed_channel(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope, GAMEPLAY_CHANNEL) -> void:
	peer.send(GAMEPLAY_CHANNEL, message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)

func loop_shadow_wrapper(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope, items: Array) -> void:
	for GAMEPLAY_CHANNEL in items:
		peer.send(GAMEPLAY_CHANNEL, message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)

func use_loop_shadow(peer: ENetPacketPeer, message: AcmeV1EnvelopeEnvelope, items: Array) -> void:
	loop_shadow_wrapper(peer, message, items)

func typed_field_shadow_wrapper(peer: ENetPacketPeer, items: Array) -> void:
	for stored_message in items:
		peer.send(12, stored_message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)

func use_typed_field_shadow(peer: ENetPacketPeer, items: Array) -> void:
	typed_field_shadow_wrapper(peer, items)

func multi_api(peer: ENetPacketPeer, connection: ENetConnection, message: AcmeV1EnvelopeEnvelope) -> void:
	peer.send(10, message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)
	connection.broadcast(11, message.to_bytes(), ENetPacketPeer.FLAG_RELIABLE)

func use_multi(peer: ENetPacketPeer, connection: ENetConnection, message: AcmeV1EnvelopeEnvelope) -> void:
	multi_api(peer, connection, message)
`)
	writeFile(t, root, "transport.gd", string(content))
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "transport.gd", Content: content, Repository: "protobuf-transport", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	useID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.use_transport").ID
	send := assertGDTransportOperation(t, result, useID, graph.EdgeSends, "send", "ENetPacketPeer.send")
	if send.Properties["channel"] != "3" || send.Properties["channel_status"] != "proven" ||
		send.Properties["reliability"] != "reliable" || send.Properties["payload_status"] != "proven" ||
		send.Properties["wrapper_depth"] != "2" {
		t.Fatalf("wrapped send evidence = %#v", send.Properties)
	}
	assertGDTransportCarries(t, result.Facts, send.ID, "acme.v1.Envelope")
	receive := assertGDTransportOperation(t, result, useID, graph.EdgeReceives, "receive", "ENetPacketPeer.get_packet")
	assertGDTransportCarries(t, result.Facts, receive.ID, "acme.v1.Envelope")

	unknown := false
	for _, node := range result.Nodes {
		if node.Kind == graph.KindTransportOperation && node.Properties["direction"] == "send" &&
			node.Properties["channel_status"] == "unknown" && node.Properties["payload_status"] == "unknown" &&
			node.Properties["reliability"] == "unknown" {
			unknown = true
		}
	}
	if !unknown {
		t.Fatalf("dynamic transport evidence missing: %#v", result.Nodes)
	}
	ambiguous := false
	unreliable := false
	for _, node := range result.Nodes {
		if node.Kind == graph.KindTransportOperation && node.Properties["channel"] == "2" && node.Properties["reliability"] == "unreliable" {
			unreliable = true
		}
		if node.Kind == graph.KindTransportOperation && node.Properties["direction"] == "send" &&
			node.Properties["channel_status"] == "ambiguous" && node.Properties["payload_status"] == "ambiguous" {
			ambiguous = true
			for _, fact := range result.Facts {
				if fact.FromID == node.ID && fact.Kind == graph.EdgeCarries {
					t.Fatalf("ambiguous wrapper payload produced carries edge: %#v", fact)
				}
			}
		}
	}
	if !ambiguous {
		t.Fatalf("conflicting wrapper evidence was not preserved as ambiguous: %#v", result.Nodes)
	}
	if !unreliable {
		t.Fatalf("exact GDScript unreliable flags were not normalized: %#v", result.Nodes)
	}
	truncated := false
	nodesByID := map[string]graph.Node{}
	for _, node := range result.Nodes {
		nodesByID[node.ID] = node
	}
	for _, fact := range result.Facts {
		node := nodesByID[fact.TargetID]
		if fact.FromID == useID && fact.Kind == graph.EdgeSends && node.Properties["channel_status"] == "truncated" &&
			node.Properties["payload_status"] == "truncated" {
			truncated = true
		}
	}
	if !truncated {
		t.Fatalf("bounded GDScript wrapper evidence did not surface truncation: %#v", result.Nodes)
	}
	ordinaryID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.ordinary").ID
	connectionID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.connection_transport").ID
	broadcast := assertGDTransportOperation(t, result, connectionID, graph.EdgeSends, "send", "ENetConnection.broadcast")
	if broadcast.Properties["channel"] != "4" || broadcast.Properties["reliability"] != "unreliable" {
		t.Fatalf("connection broadcast evidence = %#v", broadcast.Properties)
	}
	assertGDTransportCarries(t, result.Facts, broadcast.ID, "acme.v1.Envelope")
	assertGDTransportOperation(t, result, connectionID, graph.EdgeReceives, "receive", "ENetConnection.service")
	uncertainID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.uncertain_payload").ID
	agreedID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.agreed_payload").ID
	agreed := assertGDTransportOperation(t, result, agreedID, graph.EdgeSends, "send", "ENetPacketPeer.send")
	assertGDTransportCarries(t, result.Facts, agreed.ID, "acme.v1.Envelope")
	omittedID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.omitted_default").ID
	omitted := assertGDTransportOperation(t, result, omittedID, graph.EdgeSends, "send", "ENetPacketPeer.send")
	assertGDTransportDoesNotCarry(t, result.Facts, omitted.ID)
	shadowedEvidenceID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.shadowed_evidence").ID
	shadowedSend := assertGDTransportOperation(t, result, shadowedEvidenceID, graph.EdgeSends, "send", "ENetPacketPeer.send")
	assertGDTransportDoesNotCarry(t, result.Facts, shadowedSend.ID)
	shadowedReceive := assertGDTransportOperation(t, result, shadowedEvidenceID, graph.EdgeReceives, "receive", "ENetPacketPeer.get_packet")
	assertGDTransportDoesNotCarry(t, result.Facts, shadowedReceive.ID)
	shadowedChannelID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.shadowed_channel").ID
	shadowedChannel := assertGDTransportOperation(t, result, shadowedChannelID, graph.EdgeSends, "send", "ENetPacketPeer.send")
	if shadowedChannel.Properties["channel_status"] != "unknown" || shadowedChannel.Properties["channel"] != "" {
		t.Fatalf("shadowed class constant retained transport evidence: %#v", shadowedChannel.Properties)
	}
	useLoopShadowID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.use_loop_shadow").ID
	loopShadow := assertGDTransportOperation(t, result, useLoopShadowID, graph.EdgeSends, "send", "ENetPacketPeer.send")
	if loopShadow.Properties["channel_status"] != "unknown" || loopShadow.Properties["channel"] != "" {
		t.Fatalf("loop-shadowed class constant retained wrapper evidence: %#v", loopShadow.Properties)
	}
	useTypedShadowID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.use_typed_field_shadow").ID
	typedShadow := assertGDTransportOperation(t, result, useTypedShadowID, graph.EdgeSends, "send", "ENetPacketPeer.send")
	assertGDTransportDoesNotCarry(t, result.Facts, typedShadow.ID)
	useMultiID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "TransportClient.use_multi").ID
	assertGDTransportOperation(t, result, useMultiID, graph.EdgeSends, "send", "ENetPacketPeer.send")
	assertGDTransportOperation(t, result, useMultiID, graph.EdgeSends, "send", "ENetConnection.broadcast")
	wantSignature := gdTransportFactSignature(result, useMultiID)
	for iteration := 0; iteration < 32; iteration++ {
		repeated, parseErr := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
			Root: root, Path: "transport.gd", Content: content, Repository: "protobuf-transport", RepoID: "repo",
		})
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		repeatedID := findQualifiedNode(t, repeated.Nodes, graph.KindMethod, "TransportClient.use_multi").ID
		if got := gdTransportFactSignature(repeated, repeatedID); got != wantSignature {
			t.Fatalf("multi-API wrapper fact IDs are nondeterministic: want %q, got %q", wantSignature, got)
		}
	}
	for _, fact := range result.Facts {
		if fact.FromID == ordinaryID && (fact.Kind == graph.EdgeSends || fact.Kind == graph.EdgeReceives) {
			t.Fatalf("same-name non-ENet API produced transport fact: %#v", fact)
		}
		if fact.FromID == uncertainID && fact.Kind == graph.EdgeSends {
			for _, carried := range result.Facts {
				if carried.FromID == fact.TargetID && carried.Kind == graph.EdgeCarries {
					t.Fatalf("branch-dependent payload produced carries edge: %#v", carried)
				}
			}
		}
	}
}

func assertGDTransportOperation(t *testing.T, result graph.ParseResult, fromID string, relation graph.EdgeKind, direction, api string) graph.Node {
	t.Helper()
	nodes := map[string]graph.Node{}
	for _, node := range result.Nodes {
		nodes[node.ID] = node
	}
	var matched graph.Node
	for _, fact := range result.Facts {
		node := nodes[fact.TargetID]
		if fact.FromID == fromID && fact.Kind == relation && node.Kind == graph.KindTransportOperation &&
			node.Properties["direction"] == direction && node.Properties["api"] == api {
			if matched.ID == "" || node.Properties["payload_status"] == "proven" {
				matched = node
			}
		}
	}
	if matched.ID != "" {
		return matched
	}
	t.Fatalf("missing %s %s operation for %q: nodes=%#v facts=%#v diagnostics=%#v", direction, api, fromID, result.Nodes, result.Facts, result.Diagnostics)
	return graph.Node{}
}

func assertGDTransportCarries(t *testing.T, facts []graph.Fact, operationID, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.FromID == operationID && fact.Kind == graph.EdgeCarries && fact.Target == target && fact.TargetID != "" {
			return
		}
	}
	t.Fatalf("operation %q does not carry %q: %#v", operationID, target, facts)
}

func assertGDTransportDoesNotCarry(t *testing.T, facts []graph.Fact, operationID string) {
	t.Helper()
	for _, fact := range facts {
		if fact.FromID == operationID && fact.Kind == graph.EdgeCarries {
			t.Fatalf("unproven transport operation produced carries edge: %#v", fact)
		}
	}
}

func gdTransportFactSignature(result graph.ParseResult, fromID string) string {
	nodes := map[string]graph.Node{}
	for _, node := range result.Nodes {
		nodes[node.ID] = node
	}
	var facts []string
	for _, fact := range result.Facts {
		node := nodes[fact.TargetID]
		if fact.FromID == fromID && fact.Kind == graph.EdgeSends && node.Kind == graph.KindTransportOperation {
			facts = append(facts, node.Properties["api"]+"="+fact.ID)
		}
	}
	sort.Strings(facts)
	return strings.Join(facts, ",")
}

func TestParserSuppressesCorroboratedTrackedGDScriptBinding(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "buf.gen.yaml", `version: v2
plugins:
  - local: protoc-gen-gdscript
    out: gen
`)
	writeFile(t, root, "schema.proto", "syntax = \"proto3\"; message Message { string value = 1; }\n")
	content := []byte(`class_name SchemaMessage
extends RefCounted
# Generated by gdproto
# Source: schema.proto
# DO NOT EDIT
func reset() -> void:
	pass
`)
	path := "gen/SchemaMessage.pb.gd"
	writeFile(t, root, path, string(content))
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{Root: root, Path: path, Content: content, RepoID: "repo:test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Nodes) != 1 || result.Nodes[0].Kind != graph.KindFile || result.Nodes[0].Properties["generator"] != "protoc-gen-gdscript" {
		t.Fatalf("tracked binding was not deduplicated: %#v", result.Nodes)
	}
}

func TestParserExtractsCanonicalProtobufUsageWithoutGeneratedBindings(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, ".gitignore", "generated/\n")
	writeFile(t, root, "buf.yaml", "version: v2\nmodules:\n  - path: proto\n")
	writeFile(t, root, "buf.gen.yaml", `version: v2
plugins:
  - local: protoc-gen-gdscript
    out: generated
`)
	writeFile(t, root, "proto/envelope.proto", `syntax = "proto3";
package acme.v1;
message Envelope {
  string text = 1;
  repeated string tags = 2;
  Child child = 3;
  oneof payload { string raw = 4; }
}
message Child { string name = 1; }
`)
	content := []byte(`class_name Client extends Middle

class Middle extends AcmeV1EnvelopeEnvelope:
	pass

class Lookalike:
	func set_text(_value: String) -> void:
		pass
	func get_text() -> String:
		return ""
	func to_bytes() -> PackedByteArray:
		return PackedByteArray()

var stored: AcmeV1EnvelopeEnvelope
var ordinary: Node
var assigned_field
var assigned_self
var branch_field
var branch_swapped_field
var missing_else_field
var while_field
var for_field
var match_field
var shadow_field
var compound_field

class Derived extends AcmeV1EnvelopeEnvelope:
	func inherited() -> void:
		set_text("inherited")

class Override extends AcmeV1EnvelopeEnvelope:
	func set_text(_value: String) -> void:
		pass
	func get_child() -> Variant:
		return null
	func helper() -> Variant:
		return null
	func use_override() -> void:
		set_text("local override")
		var child = get_child()
		child.set_name("not a generated child")
		self.set_text("self local override")
		self.get_child().set_text("not an Envelope receiver")
		self.helper().set_text("unresolved Variant receiver")

func use(data: PackedByteArray, typed: AcmeV1EnvelopeEnvelope) -> String:
	var envelope_type = AcmeV1EnvelopeEnvelope
	var message = envelope_type.new()
	message.set_text("hello")
	message.add_tags("tag")
	var child = message.new_child()
	child.set_name("nested")
	var nested_name = message.get_child().get_name()
	if typed.has_raw():
		var raw = typed.get_raw()
		typed.set_raw(raw)
	var assigned
	assigned = typed
	var copied = assigned.get_text()
	var encoded = assigned.to_bytes()
	var decoded = AcmeV1EnvelopeEnvelope.from_bytes(data)
	decoded.set_text(copied)
	return nested_name

func forbidden(dynamic, variant: Variant, local: Lookalike) -> void:
	dynamic.set_text("unknown")
	variant.get_text()
	local.set_text("local")
	local.get_text()
	local.to_bytes()

func shadowed(AcmeV1EnvelopeEnvelope: Variant, data: PackedByteArray) -> void:
	var decoded = AcmeV1EnvelopeEnvelope.from_bytes(data)
	decoded.set_text("shadowed")

func self_qualified() -> void:
	self.stored.set_text("stored")

func self_shadowed(ordinary: AcmeV1EnvelopeEnvelope) -> void:
	self.ordinary.set_text("field remains Node")

func assigned_fields(data: PackedByteArray) -> void:
	assigned_field = AcmeV1EnvelopeEnvelope.from_bytes(data)
	self.assigned_field.set_text("bare assignment, self use")
	self.assigned_self = AcmeV1EnvelopeEnvelope.from_bytes(data)
	assigned_self.set_text("self assignment, bare use")

func assigned_field_agreed(cond: bool, data: PackedByteArray) -> void:
	if cond:
		self.assigned_field = AcmeV1EnvelopeEnvelope.from_bytes(data)
	else:
		self.assigned_field = AcmeV1EnvelopeEnvelope.from_bytes(data)
	self.assigned_field.set_text("field proven on every branch")

func uncertain_fields(cond: bool, dynamic, data: PackedByteArray, items: Array) -> void:
	if cond:
		self.branch_field = dynamic
	else:
		self.branch_field = AcmeV1EnvelopeEnvelope.from_bytes(data)
	self.branch_field.set_text("branches disagree")
	if cond:
		self.branch_swapped_field = AcmeV1EnvelopeEnvelope.from_bytes(data)
	else:
		self.branch_swapped_field = dynamic
	self.branch_swapped_field.set_text("swapped branches disagree")
	if cond:
		self.missing_else_field = AcmeV1EnvelopeEnvelope.from_bytes(data)
	self.missing_else_field.set_text("else path has no proof")
	while items.is_empty():
		self.while_field = AcmeV1EnvelopeEnvelope.from_bytes(data)
	self.while_field.set_text("loop may not run")
	for item in items:
		self.for_field = AcmeV1EnvelopeEnvelope.from_bytes(data)
	self.for_field.set_text("iteration may not run")
	match items.size():
		1:
			self.match_field = AcmeV1EnvelopeEnvelope.from_bytes(data)
	self.match_field.set_text("pattern may not match")
	var shadow_field
	shadow_field = AcmeV1EnvelopeEnvelope.from_bytes(data)
	self.shadow_field.set_text("local assignment must not prove field")
	self.compound_field += AcmeV1EnvelopeEnvelope.from_bytes(data)
	self.compound_field.set_text("compound assignment is not type proof")

func uncertain(cond: bool, dynamic, data: PackedByteArray) -> void:
	var value
	if cond:
		value = dynamic
	else:
		value = AcmeV1EnvelopeEnvelope.from_bytes(data)
	value.set_text("not proven on every branch")

func uncertain_swapped(cond: bool, dynamic, data: PackedByteArray) -> void:
	var value
	if cond:
		value = AcmeV1EnvelopeEnvelope.from_bytes(data)
	else:
		value = dynamic
	value.set_text("still not proven on every branch")

func agreed(cond: bool, data: PackedByteArray) -> void:
	var value
	if cond:
		value = AcmeV1EnvelopeEnvelope.from_bytes(data)
	else:
		value = AcmeV1EnvelopeEnvelope.from_bytes(data)
	value.set_text("proven on every branch")

func uncertain_loops(items: Array, data: PackedByteArray) -> void:
	var while_value
	while items.is_empty():
		while_value = AcmeV1EnvelopeEnvelope.from_bytes(data)
	while_value.set_text("loop may not run")
	var for_value
	for item in items:
		for_value = AcmeV1EnvelopeEnvelope.from_bytes(data)
	for_value.set_text("iteration may not run")
	var match_value
	match items.size():
		1:
			match_value = AcmeV1EnvelopeEnvelope.from_bytes(data)
	match_value.set_text("pattern may not match")

func exhaustive_match(value: int, data: PackedByteArray) -> void:
	var message
	match value:
		1:
			message = AcmeV1EnvelopeEnvelope.from_bytes(data)
		_:
			message = AcmeV1EnvelopeEnvelope.from_bytes(data)
	message.set_text("every arm proves the type")

func inherited_chain() -> void:
	set_text("inherited through inner base")

func shadow_parameter(stored) -> void:
	stored.set_text("untyped parameter")

func shadow_local() -> void:
	var stored
	stored.set_text("untyped local")

func shadow_for(items: Array) -> void:
	for stored in items:
		stored.set_text("untyped loop variable")

func shadow_lambda() -> void:
	var callback = func(stored):
		stored.set_text("untyped lambda parameter")
	callback.call(null)

func shadow_branch(cond: bool) -> void:
	if cond:
		var value: AcmeV1EnvelopeEnvelope
	else:
		var value
		value.set_text("untyped branch local")

func invalid_static_accessors() -> void:
	AcmeV1EnvelopeEnvelope.set_text("class object")
	AcmeV1EnvelopeEnvelope.to_bytes()
	var child = AcmeV1EnvelopeEnvelope.get_child()
	child.set_name("unproven return")
`)
	writeFile(t, root, "client.gd", string(content))
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client.gd", Content: content, Repository: "protobuf-gdscript", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	useID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.use").ID
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.text", "set")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.tags", "add")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.child", "new")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Child.name", "set")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeReads, "acme.v1.Envelope.child", "get")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeReads, "acme.v1.Child.name", "get")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeReads, "acme.v1.Envelope.raw", "has")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeReads, "acme.v1.Envelope.raw", "get")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.raw", "set")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeEncodes, "acme.v1.Envelope", "to_bytes")
	assertProtocolFact(t, result.Facts, useID, graph.EdgeDecodes, "acme.v1.Envelope", "from_bytes")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "AcmeV1EnvelopeEnvelope.set_text")

	forbiddenID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.forbidden").ID
	shadowedID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.shadowed").ID
	selfQualifiedID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.self_qualified").ID
	derivedID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.Derived.inherited").ID
	selfFact := assertProtocolFact(t, result.Facts, selfQualifiedID, graph.EdgeWrites, "acme.v1.Envelope.text", "set")
	if selfFact.Properties["static_type"] != "AcmeV1EnvelopeEnvelope" {
		t.Fatalf("self-qualified field static type = %q", selfFact.Properties["static_type"])
	}
	derivedFact := assertProtocolFact(t, result.Facts, derivedID, graph.EdgeWrites, "acme.v1.Envelope.text", "set")
	if derivedFact.Properties["static_type"] != "Client.Derived" {
		t.Fatalf("derived receiver static type = %q", derivedFact.Properties["static_type"])
	}
	agreedID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.agreed").ID
	assertProtocolFact(t, result.Facts, agreedID, graph.EdgeWrites, "acme.v1.Envelope.text", "set")
	assignedFieldsID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.assigned_fields").ID
	assertProtocolFactCount(t, result.Facts, assignedFieldsID, graph.EdgeWrites, "acme.v1.Envelope.text", "set", 2)
	assignedFieldAgreedID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.assigned_field_agreed").ID
	assertProtocolFact(t, result.Facts, assignedFieldAgreedID, graph.EdgeWrites, "acme.v1.Envelope.text", "set")
	exhaustiveMatchID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.exhaustive_match").ID
	assertProtocolFact(t, result.Facts, exhaustiveMatchID, graph.EdgeWrites, "acme.v1.Envelope.text", "set")
	inheritedChainID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.inherited_chain").ID
	assertProtocolFact(t, result.Facts, inheritedChainID, graph.EdgeWrites, "acme.v1.Envelope.text", "set")
	uncertainID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.uncertain").ID
	uncertainSwappedID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.uncertain_swapped").ID
	uncertainLoopsID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.uncertain_loops").ID
	uncertainFieldsID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.uncertain_fields").ID
	selfShadowedID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.self_shadowed").ID
	overrideID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Client.Override.use_override").ID
	negativeMethods := map[string]bool{}
	for _, qualified := range []string{"Client.shadow_parameter", "Client.shadow_local", "Client.shadow_for", "Client.shadow_lambda", "Client.shadow_branch", "Client.invalid_static_accessors"} {
		negativeMethods[findQualifiedNode(t, result.Nodes, graph.KindMethod, qualified).ID] = true
	}
	for _, fact := range result.Facts {
		if (fact.FromID == forbiddenID || fact.FromID == shadowedID || fact.FromID == selfShadowedID || fact.FromID == overrideID || negativeMethods[fact.FromID]) && fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("unproven or local lookalike receiver produced protocol usage: %#v", fact)
		}
		if (fact.FromID == uncertainID || fact.FromID == uncertainSwappedID || fact.FromID == uncertainLoopsID || fact.FromID == uncertainFieldsID) && fact.Kind == graph.EdgeWrites && fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("branch-dependent receiver produced protocol write: %#v", fact)
		}
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "Client.Override.set_text")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "Node.set_text")
}

func TestParserInfersForwardFieldThroughGeneratedBase(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "buf.gen.yaml", "version: v2\nplugins:\n  - local: protoc-gen-gdscript\n    out: generated\n")
	writeFile(t, root, "envelope.proto", "syntax = \"proto3\"; package acme.v1; message Envelope { string text = 1; }\n")
	content := []byte(`class_name Forward extends AcmeV1EnvelopeEnvelope

func use() -> void:
	cached.set_text("forward")

var cached = from_bytes(PackedByteArray())
`)
	writeFile(t, root, "forward.gd", string(content))
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "forward.gd", Content: content, RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	useID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Forward.use").ID
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.text", "set")
}

func TestParserRejectsShadowedGeneratedBaseForForwardField(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "buf.gen.yaml", "version: v2\nplugins:\n  - local: protoc-gen-gdscript\n    out: generated\n")
	writeFile(t, root, "envelope.proto", "syntax = \"proto3\"; package acme.v1; message Envelope { string text = 1; }\n")
	content := []byte(`class_name Shadow extends AcmeV1EnvelopeEnvelope

class AcmeV1EnvelopeEnvelope:
	pass

func use() -> void:
	cached.set_text("not a generated message")

var cached = from_bytes(PackedByteArray())
`)
	writeFile(t, root, "shadow.gd", string(content))
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "shadow.gd", Content: content, RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	useID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Shadow.use").ID
	for _, fact := range result.Facts {
		if fact.FromID == useID && fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("shadowed generated base produced protocol usage: %#v", fact)
		}
	}
}

func TestParserInfersForwardFieldThroughInnerGeneratedBase(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "buf.gen.yaml", "version: v2\nplugins:\n  - local: protoc-gen-gdscript\n    out: generated\n")
	writeFile(t, root, "envelope.proto", "syntax = \"proto3\"; package acme.v1; message Envelope { string text = 1; }\n")
	content := []byte(`class_name Multi extends Mid

class Mid extends AcmeV1EnvelopeEnvelope:
	pass

func use() -> void:
	cached.set_text("forward through inner base")

var cached = from_bytes(PackedByteArray())
`)
	writeFile(t, root, "multi.gd", string(content))
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "multi.gd", Content: content, RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	useID := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Multi.use").ID
	assertProtocolFact(t, result.Facts, useID, graph.EdgeWrites, "acme.v1.Envelope.text", "set")
}

func TestParserRejectsAmbiguousProtobufGDScriptBindings(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "buf.yaml", "version: v2\nmodules:\n  - path: proto\n")
	writeFile(t, root, "buf.gen.yaml", "version: v2\nplugins:\n  - local: protoc-gen-gdscript\n    out: generated\n")
	writeFile(t, root, "proto/one/envelope.proto", "syntax = \"proto3\"; package acme.v1; message Envelope { string text = 1; }\n")
	// gdproto's class-name normalization makes acme.v1 and acme_v1 collide.
	writeFile(t, root, "proto/two/envelope.proto", "syntax = \"proto3\"; package acme_v1; message Envelope { string other = 1; }\n")
	content := []byte("class_name Client\nclass Derived extends AcmeV1EnvelopeEnvelope:\n\tfunc use() -> void:\n\t\tset_text(\"ambiguous base\")\nfunc use(typed: AcmeV1EnvelopeEnvelope) -> void:\n\ttyped.set_text(\"first schema\")\n\ttyped.set_other(\"second schema\")\n\tvar inferred = AcmeV1EnvelopeEnvelope.new()\n\tinferred.set_text(\"constructor fallback\")\n")
	writeFile(t, root, "client.gd", string(content))
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client.gd", Content: content, RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		if fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("ambiguous generated API produced protocol usage: %#v", fact)
		}
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "AcmeV1EnvelopeEnvelope.set_text")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "AcmeV1EnvelopeEnvelope.set_other")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "inferred.set_text")
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Message, "ambiguous generated Protobuf GDScript API") {
			return
		}
	}
	t.Fatalf("missing ambiguous binding diagnostic: %#v", result.Diagnostics)
}

func TestParserSuppressesProtocolUseInConfiguredGDScriptOutputWithRejectedHeader(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "buf.gen.yaml", "version: v2\nplugins:\n  - local: protoc-gen-gdscript\n    out: generated\n")
	writeFile(t, root, "schema.proto", "syntax = \"proto3\"; message Message { string value = 1; }\n")
	content := []byte("class_name SchemaMessage\n# Generated by an unsupported tool\nfunc use(value: SchemaMessage) -> void:\n\tvalue.set_value(\"generated implementation\")\n")
	path := "generated/SchemaMessage.pb.gd"
	writeFile(t, root, path, string(content))
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{Root: root, Path: path, Content: content, RepoID: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "SchemaMessage.set_value")
	for _, fact := range result.Facts {
		if fact.Properties["protocol"] == "protobuf" {
			t.Fatalf("configured generated implementation produced protocol usage: %#v", fact)
		}
	}
}

func assertProtocolFact(t *testing.T, facts []graph.Fact, fromID string, kind graph.EdgeKind, target, form string) graph.Fact {
	t.Helper()
	for _, fact := range facts {
		if fact.FromID != fromID || fact.Kind != kind || fact.Target != target || fact.Properties["form"] != form {
			continue
		}
		if fact.TargetID == "" || fact.Location.Path == "" || fact.Location.Line == 0 ||
			fact.Properties["protocol"] != "protobuf" || fact.Properties["api"] == "" ||
			fact.Properties["static_type"] == "" || fact.Properties["binding"] == "" ||
			fact.Properties["binding_id"] == "" || fact.Properties["evidence"] != "gdscript_scope" {
			t.Fatalf("protocol fact lost canonical evidence: %#v", fact)
		}
		return fact
	}
	t.Fatalf("missing %s protocol fact from %q to %q with form %q", kind, fromID, target, form)
	return graph.Fact{}
}

func assertProtocolFactCount(t *testing.T, facts []graph.Fact, fromID string, kind graph.EdgeKind, target, form string, want int) {
	t.Helper()
	count := 0
	for _, fact := range facts {
		if fact.FromID == fromID && fact.Kind == kind && fact.Target == target && fact.Properties["form"] == form {
			count++
		}
	}
	if count != want {
		t.Fatalf("%s protocol fact count from %q to %q with form %q = %d, want %d", kind, fromID, target, form, count, want)
	}
}

func TestParserCreatesImplicitScriptClassAndInnerTypes(t *testing.T) {
	content := []byte(`extends Node

enum State { IDLE, RUNNING = 2 }

class Helper extends RefCounted:
	var enabled: bool
	func run() -> void:
		pass

func ready() -> void:
	for child: Node in get_children():
		child.queue_free()
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/controller.gd", Content: content, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasQualifiedNode(t, result.Nodes, graph.KindClass, "scripts/controller")
	assertHasQualifiedNode(t, result.Nodes, graph.KindClass, "scripts/controller.Helper")
	assertHasNode(t, result.Nodes, graph.KindType, "State")
	assertHasNode(t, result.Nodes, graph.KindVariable, "child")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "Node")
	assertHasFact(t, result.Facts, graph.EdgeExtends, "RefCounted")
}

func TestParserResolvesMembersDeclaredLater(t *testing.T) {
	content := []byte(`class_name Ordered

func start(value: int) -> void:
	later = value
	finished.emit(value)
	finish()

var later: int
signal finished(value: int)
func finish() -> void:
	pass
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "ordered.gd", Content: content, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "Ordered.finish")
	assertHasFactKind(t, result.Facts, graph.EdgeAssigns)
	assertHasFactKind(t, result.Facts, graph.EdgePublishes)
}

func TestParserReturnsPositionedSyntaxErrors(t *testing.T) {
	_, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "broken.gd", Content: []byte("func broken(\n"),
	})
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "broken.gd:") {
		t.Fatalf("error does not contain filename and position: %v", err)
	}
}

func TestParserSupportsGDScriptFilesCaseInsensitively(t *testing.T) {
	parser := gdscriptparser.New()
	if !parser.Supports("player.gd") || !parser.Supports("PLAYER.GD") || parser.Supports("player.gdshader") {
		t.Fatal("unexpected GDScript extension support")
	}
}

func TestParserLinksProjectSettingsAndInputActions(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/settings.gd", RepoID: "repo:sample", Content: []byte(`class_name Settings

func load_main_scene() -> String:
	if Input.is_action_just_pressed("jump"):
		return "jump"
	return ProjectSettings.get_setting("application/run/main_scene")
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "application/run/main_scene")
	assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "input/jump")
}

func TestParserLinksNodePathAndRuntimeNodeLookups(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/menu.gd", RepoID: "repo:sample", Content: []byte(`class_name Menu

func bind_nodes() -> void:
	var unique_button = %StartButton
	var status = $Panel/Status
	get_node("Panel/StartButton")
	get_node_or_null(^"Panel/Optional")
	has_node("Panel/Status:visible")
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFactWithProperty(t, result.Facts, graph.EdgeReferences, "StartButton", "unique", "true")
	assertHasFactWithProperty(t, result.Facts, graph.EdgeReferences, "StartButton", "lookup", "get_node")
	assertHasFactWithProperty(t, result.Facts, graph.EdgeReferences, "Status", "form", "node_path")
	assertHasFactWithProperty(t, result.Facts, graph.EdgeReferences, "Optional", "lookup", "get_node_or_null")
	assertHasFactWithProperty(t, result.Facts, graph.EdgeReferences, "Status", "lookup", "has_node")
}

func assertHasFactKind(t *testing.T, facts []graph.Fact, kind graph.EdgeKind) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind {
			return
		}
	}
	t.Fatalf("missing %s fact; got %#v", kind, facts)
}

func assertHasNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, name string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.Name == name {
			return
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, name, nodes)
}

func assertHasQualifiedNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, qualified, nodes)
}

func assertHasFact(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return
		}
	}
	t.Fatalf("missing %s fact to %q; got %#v", kind, target, facts)
}

func assertHasFactWithProperty(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target, key, value string) {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target && fact.Properties[key] == value {
			return
		}
	}
	t.Fatalf("missing %s fact to %q with %s=%q; got %#v", kind, target, key, value, facts)
}

func TestParserDeclaresScriptModuleForResourceIdentity(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/player.gd", Content: []byte("class_name Player extends Node\n"),
		Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	module := findQualifiedNode(t, result.Nodes, graph.KindModule, "scripts/player")
	if module.Properties["form"] != "script" {
		t.Fatalf("script module properties = %#v", module.Properties)
	}
	class := findQualifiedNode(t, result.Nodes, graph.KindClass, "Player")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeDeclares && fact.FromID == module.ID && fact.TargetID == class.ID {
			return
		}
	}
	t.Fatalf("script module does not declare its class; got %#v", result.Facts)
}

func TestParserResolvesAutoloadUsesFromProjectDeclarations(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "client/project.godot", `config_version=5

[autoload]
Game="*res://scripts/game.gd"
Menu="*res://scenes/menu.tscn"
Disabled="res://scripts/disabled.gd"
Twice="*res://scripts/a.gd"
Twice="*res://scripts/b.gd"
`)
	content := []byte(`extends Node

func ready() -> void:
	Game.start()
	var scene = Menu
	Disabled.start()
	Twice.start()
	Unknown.start()
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/hud.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	call := findFactWithTarget(t, result.Facts, graph.EdgeReferences, "godot:autoload:client/project.godot:Game")
	if call.TargetKind != graph.KindGodotAutoload || call.Properties["form"] != "autoload_call" ||
		call.Properties["member"] != "start" {
		t.Fatalf("autoload call fact = %#v", call)
	}
	reference := findFactWithTarget(t, result.Facts, graph.EdgeReferences, "godot:autoload:client/project.godot:Menu")
	if reference.Properties["form"] != "autoload_reference" {
		t.Fatalf("autoload reference fact = %#v", reference)
	}
	// A conflicting declaration, an undeclared name, and an autoload Godot does
	// not expose as a global singleton must all resolve nothing.
	for _, name := range []string{"Twice", "Unknown", "Disabled"} {
		for _, fact := range result.Facts {
			if strings.HasSuffix(fact.Target, ":"+name) && strings.HasPrefix(fact.Target, "godot:autoload:") {
				t.Fatalf("autoload %q must not resolve: %#v", name, fact)
			}
		}
	}
}

func TestParserKeepsProjectOwnershipInsideSourceMembership(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "project.godot", "[autoload]\nRoot=\"*res://scripts/root.gd\"\n")
	writeFile(t, root, "nested/project.godot", "[autoload]\nNested=\"*res://scripts/nested.gd\"\n")
	input := parserapi.Input{
		Root: root, Path: "nested/scripts/hud.gd", SourcePaths: []string{"project.godot", "nested/scripts/hud.gd"},
		Content:    []byte("extends Node\nfunc ready():\n\tRoot.start()\n\tNested.start()\n"),
		Repository: "sample", RepoID: "repo:sample",
	}
	parser := gdscriptparser.New()
	before, err := parser.SemanticKey(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := parser.Parse(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	findFactWithTarget(t, result.Facts, graph.EdgeReferences, "godot:autoload:project.godot:Root")
	for _, fact := range result.Facts {
		if fact.Target == "godot:autoload:nested/project.godot:Nested" {
			t.Fatalf("excluded nested project owned eligible GDScript: %#v", fact)
		}
	}

	writeFile(t, root, "nested/project.godot", "[autoload]\nChanged=\"*res://scripts/changed.gd\"\n")
	after, err := parser.SemanticKey(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("excluded project changed GDScript semantic key: before=%q after=%q", before, after)
	}
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findQualifiedNode(t *testing.T, nodes []graph.Node, kind graph.NodeKind, qualified string) graph.Node {
	t.Helper()
	for _, node := range nodes {
		if node.Kind == kind && node.QualifiedName == qualified {
			return node
		}
	}
	t.Fatalf("missing %s node %q; got %#v", kind, qualified, nodes)
	return graph.Node{}
}

func findFactWithTarget(t *testing.T, facts []graph.Fact, kind graph.EdgeKind, target string) graph.Fact {
	t.Helper()
	for _, fact := range facts {
		if fact.Kind == kind && fact.Target == target {
			return fact
		}
	}
	t.Fatalf("missing %s fact to %q; got %#v", kind, target, facts)
	return graph.Fact{}
}

// TestParserKeepsProjectEscapingPreloadsUnresolved covers the project boundary on
// the script side: a preload that traverses out of its own project must not
// resolve into a sibling project.
func TestParserKeepsProjectEscapingPreloadsUnresolved(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "client/project.godot", "config_version=5\n")
	writeFile(t, root, "tools/probe/project.godot", "config_version=5\n")

	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/hud.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte("extends Node\n" +
			"const Inside = preload('res://scripts/inside.gd')\n" +
			"const Outside = preload('res://../tools/probe/scripts/probe.gd')\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeImports, "client/scripts/inside")
	for _, fact := range result.Facts {
		if strings.HasPrefix(fact.Target, "tools/probe") {
			t.Fatalf("a preload escaped its project: %#v", fact)
		}
		if fact.Kind == graph.EdgeImports && fact.Target == "" {
			t.Fatalf("an unresolvable preload emitted an empty target: %#v", fact)
		}
	}
}

// TestParserLinksLiteralInputActionUses covers the typed action vocabulary: a
// literal action name on a recognized action API resolves to the project-scoped
// action, a computed name resolves nothing, and a literal argument that is not an
// action name on an Input call is no longer mistaken for one.
func TestParserLinksLiteralInputActionUses(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "client/project.godot", "config_version=5\n\n[input]\njump={\"deadzone\": 0.5, \"events\": []}\n")
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/hud.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`extends Node

const ACTION = "crouch"

func poll(event: InputEvent) -> void:
	if Input.is_action_just_pressed("jump"):
		pass
	var strength = Input.get_action_strength("attack")
	var lean = Input.get_axis("lean_left", "lean_right")
	if event.is_action_pressed("cancel"):
		pass
	if InputMap.has_action(ACTION):
		pass
	Input.set_custom_mouse_cursor("res://art/cursor.png")
	if is_action_bar_visible():
		pass

func is_action_bar_visible() -> bool:
	return true
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct{ action, form string }{
		{action: "jump", form: "query"},
		{action: "attack", form: "query"},
		{action: "lean_left", form: "query"},
		{action: "lean_right", form: "query"},
		{action: "cancel", form: "query"},
	} {
		target := "godot:input_action:client/project.godot:" + testCase.action
		fact := findFactWithTarget(t, result.Facts, graph.EdgeUsesInputAction, target)
		if fact.TargetKind != graph.KindGodotInputAction || fact.Properties["form"] != testCase.form ||
			fact.Properties["action"] != testCase.action {
			t.Fatalf("input action fact = %#v", fact)
		}
		// The generic configuration reader stays, so config catalogs keep the
		// same evidence they had before actions became their own kind.
		assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "input/"+testCase.action)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeUsesInputAction && fact.Target == "" {
			t.Fatalf("an action use emitted an empty target: %#v", fact)
		}
		if fact.Kind != graph.EdgeUsesInputAction {
			continue
		}
		switch fact.Properties["action"] {
		case "crouch":
			t.Fatalf("a computed action name must resolve nothing: %#v", fact)
		case "res://art/cursor.png":
			t.Fatalf("a non-action argument must not become an action: %#v", fact)
		}
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeReadsConfig && strings.HasPrefix(fact.Target, "input/res://") {
			t.Fatalf("a cursor path must not be read as an input action: %#v", fact)
		}
	}
}

// TestParserLinksLiteralGroupOperations covers the node-group vocabulary:
// membership, membership tests, lookups, and dispatches each keep their operation
// form, a dispatch records its method as evidence without inventing a handler, and
// a computed group name resolves nothing.
func TestParserLinksLiteralGroupOperations(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "client/project.godot", "config_version=5\n\n[global_group]\nenemies=\"Hostile\"\n")
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/spawner.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`extends Node

const OPT_OUT = "opted_out"

func wire(other: Node) -> void:
	add_to_group("enemies")
	other.add_to_group("damageable")
	other.remove_from_group("damageable")
	if other.is_in_group("enemies"):
		pass
	var all = get_tree().get_nodes_in_group("enemies")
	get_tree().call_group("enemies", "die")
	get_tree().notify_group("enemies", 1)
	other.add_to_group(OPT_OUT)
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	enemies := "godot:node_group:client/project.godot:enemies"
	damageable := "godot:node_group:client/project.godot:damageable"
	for _, testCase := range []struct {
		kind   graph.EdgeKind
		target string
		form   string
	}{
		{kind: graph.EdgeInGroup, target: enemies, form: "add"},
		{kind: graph.EdgeInGroup, target: damageable, form: "add"},
		{kind: graph.EdgeInGroup, target: damageable, form: "remove"},
		{kind: graph.EdgeUsesGroup, target: enemies, form: "membership_test"},
		{kind: graph.EdgeUsesGroup, target: enemies, form: "lookup"},
		{kind: graph.EdgeUsesGroup, target: enemies, form: "call"},
		{kind: graph.EdgeUsesGroup, target: enemies, form: "notify"},
	} {
		assertHasFactWithProperty(t, result.Facts, testCase.kind, testCase.target, "form", testCase.form)
	}
	// A membership test is never membership evidence.
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeInGroup && fact.Properties["form"] == "membership_test" {
			t.Fatalf("is_in_group must not assert membership: %#v", fact)
		}
		if fact.Kind == graph.EdgeUsesGroup || fact.Kind == graph.EdgeInGroup {
			if fact.Target == "" {
				t.Fatalf("a group operation emitted an empty target: %#v", fact)
			}
			if fact.TargetKind != graph.KindGodotNodeGroup {
				t.Fatalf("a group operation must target a node group: %#v", fact)
			}
			if fact.Properties["group"] == "" {
				t.Fatalf("a group operation is missing its name evidence: %#v", fact)
			}
		}
		if strings.HasSuffix(fact.Target, ":opted_out") {
			t.Fatalf("a computed group name must resolve nothing: %#v", fact)
		}
	}
	// A group dispatch keeps its literal method as evidence and never becomes a
	// call edge to a guessed receiver.
	assertHasFactWithProperty(t, result.Facts, graph.EdgeUsesGroup, enemies, "method", "die")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeCalls && fact.Target == "die" {
			t.Fatalf("a group dispatch must not guess a handler: %#v", fact)
		}
	}
}

// TestParserRoutesLiteralSignalConnections covers signal routing from scripts:
// connect subscribes and names its handler, emit publishes, and disconnect and
// is_connected are routing evidence that never become subscriptions.
func TestParserRoutesLiteralSignalConnections(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/hud.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Hud extends Node

signal ready_changed(value: bool)

func wire() -> void:
	ready_changed.connect(on_ready_changed)
	ready_changed.emit(true)
	if ready_changed.is_connected(on_ready_changed):
		ready_changed.disconnect(on_ready_changed)
	emit_signal("ready_changed", false)

func on_ready_changed(_value: bool) -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	event := findQualifiedNode(t, result.Nodes, graph.KindEvent, "Hud.ready_changed")
	handled := false
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeHandledBy && fact.FromID == event.ID &&
			fact.Target == "Hud.on_ready_changed" && fact.Properties["form"] == "connect" {
			handled = true
		}
		if fact.Kind == graph.EdgeSubscribes && fact.Properties["form"] != "connect" {
			t.Fatalf("only a connect may subscribe: %#v", fact)
		}
	}
	if !handled {
		t.Fatalf("a literal connect must name its handler; got %#v", result.Facts)
	}
	// The route's destination is recorded on the routing fact as well, which is
	// the only place it can live when the signal's owner is another file.
	subscribed := false
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeSubscribes && fact.Properties["handler"] == "Hud.on_ready_changed" {
			subscribed = true
		}
	}
	if !subscribed {
		t.Fatalf("a connect must record its handler as evidence; got %#v", result.Facts)
	}
	for _, form := range []string{"signal_disconnect", "signal_connection_test"} {
		found := false
		for _, fact := range result.Facts {
			if fact.Kind == graph.EdgeReferences && fact.TargetID == event.ID &&
				fact.Properties["form"] == form {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing %s evidence; got %#v", form, result.Facts)
		}
	}
	publishes := 0
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgePublishes && fact.TargetID == event.ID {
			publishes++
			if fact.Properties["form"] != "emit" {
				t.Fatalf("a publish must record its form: %#v", fact)
			}
		}
	}
	if publishes != 2 {
		t.Fatalf("expected both emit forms to publish, got %d", publishes)
	}
}

func TestParserProjectsConfiguredCallEffectsAndPreservesCalls(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "grafo.yaml", `adapters:
  - match: {language: gdscript, symbol: Signals.wire}
    effects:
      - kind: event.subscribe
        roles: {event: {argument: 0}, handler: {argument: 1}}
  - match: {language: gdscript, symbol: Signals.unwire}
    effects:
      - kind: event.unsubscribe
        roles: {event: {argument: 0}, handler: {argument: 1}}
  - match: {language: gdscript, symbol: Signals.connected}
    effects:
      - kind: event.connection_test
        roles: {event: {argument: 0}, handler: {argument: 1}}
  - match: {language: gdscript, symbol: Signals.publish}
    effects:
      - kind: event.publish
        roles: {event: {argument: 0}}
  - match: {language: gdscript, symbol: AuthAPI.send}
    effects:
      - kind: http.request
        roles: {method: {argument: 0}, url: {argument: 1}}
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "scripts/hub.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Hub extends Node

signal round_started(value: int)

func configure(auth: AuthAPI) -> void:
	Signals.wire(round_started, on_round_started)
	Signals.wire(round_started, func(_value): pass)
	Signals.wire(round_started, on_round_started.bind(1))
	round_started.connect(on_round_started)
	Signals.unwire(round_started, on_round_started)
	Signals.connected(round_started, on_round_started)
	Signals.publish(round_started)
	auth.send(HTTPClient.METHOD_POST, "/rounds")

func on_round_started(_value: int) -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"Signals.wire", "Signals.unwire", "Signals.connected", "Signals.publish", "AuthAPI.send"} {
		assertHasFact(t, result.Facts, graph.EdgeCalls, target)
	}
	subscriptions := 0
	handled := 0
	var native, configured graph.Fact
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeSubscribes && (fact.Target == "Hub.round_started" || fact.TargetID != "") {
			subscriptions++
			if fact.Properties["adapter_symbol"] == "Signals.wire" && fact.Properties["handler"] == "Hub.on_round_started" {
				configured = fact
			} else if fact.Properties["handler"] == "Hub.on_round_started" {
				native = fact
			}
			if fact.Properties["adapter_symbol"] != "" &&
				(fact.Properties["adapter_symbol"] != "Signals.wire" || !strings.HasPrefix(fact.Properties["adapter_source"], "grafo.yaml:")) {
				t.Fatalf("configured subscription provenance = %#v", fact.Properties)
			}
		}
		if fact.Kind == graph.EdgeHandledBy && fact.Target == "Hub.on_round_started" {
			handled++
			if fact.Properties["adapter_symbol"] != "" && fact.Properties["adapter_symbol"] != "Signals.wire" {
				t.Fatalf("configured handled_by provenance = %#v", fact.Properties)
			}
		}
	}
	if subscriptions != 4 || handled != 2 {
		t.Fatalf("configured signal routes: subscriptions=%d handled=%d facts=%#v", subscriptions, handled, result.Facts)
	}
	if native.TargetID == "" || native.TargetID != configured.TargetID || native.FromID != configured.FromID ||
		native.Properties["form"] != configured.Properties["form"] || native.Properties["signal"] != configured.Properties["signal"] {
		t.Fatalf("native/configured subscriptions diverged: native=%#v configured=%#v", native, configured)
	}
	wantForms := map[string]bool{"emit": false, "signal_disconnect": false, "signal_connection_test": false}
	for _, fact := range result.Facts {
		if fact.Properties["signal"] != "Hub.round_started" {
			continue
		}
		if _, wanted := wantForms[fact.Properties["form"]]; wanted {
			wantForms[fact.Properties["form"]] = true
		}
	}
	for form, found := range wantForms {
		if !found {
			t.Fatalf("missing configured signal form %q: %#v", form, result.Facts)
		}
	}
	request := findFactWithTarget(t, result.Facts, graph.EdgeRequests, "POST /rounds")
	if request.Properties["adapter_symbol"] != "AuthAPI.send" || request.Properties["http_signature"] != "configured" {
		t.Fatalf("configured HTTP request = %#v", request)
	}
}

func TestParserConfiguredCallEffectsFailClosed(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "grafo.yaml", `adapters:
  - match: {language: gdscript, symbol: Signals.wire}
    effects:
      - kind: event.subscribe
        roles: {event: {argument: 0}, handler: {argument: 1}}
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "scripts/hub.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`extends Node
signal changed
func run():
	Signals.wire(changed)
	var Signals = get_node("Signals")
	Signals.wire(changed, callback)
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeSubscribes || fact.Kind == graph.EdgeHandledBy {
			t.Fatalf("invalid or shadowed adapter call emitted signal semantics: %#v", fact)
		}
	}
	wantDiagnostics := map[string]bool{"requires handler argument 1": false, "could not be resolved uniquely": false}
	for _, diagnostic := range result.Diagnostics {
		for text := range wantDiagnostics {
			if strings.Contains(diagnostic.Message, text) {
				wantDiagnostics[text] = true
			}
		}
	}
	for text, found := range wantDiagnostics {
		if !found {
			t.Fatalf("missing %q diagnostic: %#v", text, result.Diagnostics)
		}
	}
}

func TestConfiguredEffectRetainsOrdinaryCallAcrossBuiltinEarlyReturn(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "grafo.yaml", `adapters:
  - match: {language: gdscript, symbol: Assets.load}
    effects:
      - kind: event.publish
        roles: {event: {argument: 0}}
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "scripts/loader.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Loader extends Node
signal completed
func run() -> void:
	Assets.load(completed)
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "Assets.load")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgePublishes && fact.Properties["signal"] == "Loader.completed" &&
			fact.Properties["adapter_symbol"] == "Assets.load" {
			return
		}
	}
	t.Fatalf("configured early-return call did not publish: %#v", result.Facts)
}

// TestParserRecordsHandlerWhenSignalOwnerIsAnotherFile is the case the smoke run
// exposed: nearly every real connect names a signal another file declares, so no
// handled_by edge is emitted with a named source when this parser cannot see the
// declaration. Storage, rather than the parser, decides whether that source is
// uniquely declared.
func TestParserRecordsHandlerWhenSignalOwnerIsAnotherFile(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/kit.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Kit extends Node

var _backend: Backend

func wire() -> void:
	_backend.sign_in_success.connect(_on_sign_in)
	_backend.sign_in_failed.connect(func(): pass)

func _on_sign_in() -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	routed := findFactWithTarget(t, result.Facts, graph.EdgeSubscribes, "Backend.sign_in_success")
	if routed.Properties["form"] != "connect" || routed.Properties["handler"] != "Kit._on_sign_in" {
		t.Fatalf("cross-file connect fact = %#v", routed)
	}
	lambda := findFactWithTarget(t, result.Facts, graph.EdgeSubscribes, "Backend.sign_in_failed")
	if _, ok := lambda.Properties["handler"]; ok {
		t.Fatalf("a lambda proves no method; got %#v", lambda)
	}
	handled := findFactWithTarget(t, result.Facts, graph.EdgeHandledBy, "Kit._on_sign_in")
	if handled.FromID != "" || handled.Source != "Backend.sign_in_success" ||
		handled.SourceKind != graph.KindEvent || handled.Properties["form"] != "connect" {
		t.Fatalf("cross-file handled_by fact = %#v", handled)
	}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeHandledBy && fact.Source == "Backend.sign_in_failed" {
			t.Fatalf("a lambda proves no handler: %#v", fact)
		}
	}
}

// TestParserDoesNotReadANonSignalLiteralAsASignalName covers the ambiguity a
// project can create for itself: a connect(url) of its own must not name an event
// after its argument, and a disconnect on an object whose type is unknown must not
// invent a signal either.
func TestParserDoesNotReadANonSignalLiteralAsASignalName(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/socket.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Socket extends Node

var _client: WebSocketPeer

func open() -> void:
	_client.connect("wss://example.invalid/stream")
	inherited_signal.emit(42)
	inherited_signal.connect(_on_inherited)

func _on_inherited() -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		if fact.TargetKind == graph.KindEvent && strings.Contains(fact.Target, "://") {
			t.Fatalf("a URL must never become a signal: %#v", fact)
		}
	}
	// Nor does the receiver become one: a literal that can be neither a Callable
	// nor a signal name is proof this is not a signal connection, so the call
	// stays an ordinary call.
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeSubscribes && fact.Target != "inherited_signal" {
			t.Fatalf("a connect(url) must not subscribe to anything: %#v", fact)
		}
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "WebSocketPeer.connect")
	// A signal a base class declares is emitted and connected by its bare name.
	// It cannot be proved here, so it stays an unresolved event that the graph
	// resolves by name only when exactly one declaration owns it.
	assertHasFactWithProperty(t, result.Facts, graph.EdgePublishes, "inherited_signal", "form", "emit")
	assertHasFactWithProperty(t, result.Facts, graph.EdgeSubscribes, "inherited_signal", "handler",
		"Socket._on_inherited")
}

func TestParserQualifiesLegacyObjectSignalHandlersWithoutGuessing(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/kit.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Kit extends Node

var _backend: Backend
var _unknown

func wire() -> void:
	_backend.connect("ready", _on_ready)
	_unknown.connect("ready", _on_ready)

func _on_ready() -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for _, fact := range result.Facts {
		if fact.Kind != graph.EdgeHandledBy {
			continue
		}
		want++
		if fact.Source != "Backend.ready" || fact.SourceKind != graph.KindEvent ||
			fact.Target != "Kit._on_ready" {
			t.Fatalf("legacy Object.connect handled_by fact = %#v", fact)
		}
	}
	if want != 1 {
		t.Fatalf("legacy Object.connect handled_by facts = %d, want 1; got %#v", want, result.Facts)
	}
}

func TestParserRequiresStructuralOwnerForMemberSignalHandlers(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/kit.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Kit extends Node

var _backend: Backend
var _unknown

func wire() -> void:
	_backend.ready.connect(_on_typed)
	_unknown.ready.connect(_on_unknown)

func _on_typed() -> void:
	pass

func _on_unknown() -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	typed := false
	for _, fact := range result.Facts {
		if fact.Kind != graph.EdgeHandledBy {
			continue
		}
		if fact.Target == "Kit._on_unknown" {
			t.Fatalf("untyped member receiver produced handled_by fact %#v", fact)
		}
		if fact.Target != "Kit._on_typed" || fact.Source != "Backend.ready" ||
			fact.SourceKind != graph.KindEvent {
			t.Fatalf("member signal handled_by fact = %#v", fact)
		}
		typed = true
	}
	if !typed {
		t.Fatalf("typed member signal produced no handled_by fact: %#v", result.Facts)
	}
}

// TestParserKeepsLocallyDeclaredActionMethodsOutOfTheVocabulary covers both
// spellings of a call on this object. A script is free to declare its own
// is_action_pressed, and neither the bare nor the self-qualified call to it
// involves a Godot input API, so neither may produce a typed action edge.
func TestParserKeepsLocallyDeclaredActionMethodsOutOfTheVocabulary(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "client/project.godot", "config_version=5\n\n[input]\njump={\"deadzone\": 0.5, \"events\": []}\n")
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/pane.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Pane extends Control

func poll() -> void:
	if is_action_pressed("jump"):
		pass
	if self.is_action_pressed("jump"):
		pass
	if Pane.is_action_pressed("jump"):
		pass
	if add_to_group("enemies"):
		pass
	if self.add_to_group("enemies"):
		pass

func is_action_pressed(_name: String) -> bool:
	return false

func add_to_group(_name: String) -> bool:
	return false
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range result.Facts {
		switch fact.Kind {
		case graph.EdgeUsesInputAction, graph.EdgeInGroup, graph.EdgeUsesGroup:
			t.Fatalf("a locally declared method must not produce a Godot interaction: %#v", fact)
		case graph.EdgeReadsConfig:
			if strings.HasPrefix(fact.Target, "input/") {
				t.Fatalf("a locally declared method must not read an input action: %#v", fact)
			}
		}
	}
}

// TestParserResolvesActionQueriesOnInputEventReceivers covers the InputEvent
// action-query methods. is_action is an InputEvent method rather than an Input or
// InputMap one, so requiring an Input receiver discarded correct code.
func TestParserResolvesActionQueriesOnInputEventReceivers(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "client/project.godot", "config_version=5\n")
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/input.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`extends Node

func _input(event: InputEvent) -> void:
	if event.is_action("ui_accept"):
		pass
	if event.is_action_pressed("ui_cancel"):
		pass
	if event.is_action_released("ui_left"):
		pass
	var strength = event.get_action_strength("ui_right")
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"ui_accept", "ui_cancel", "ui_left", "ui_right"} {
		fact := findFactWithTarget(t, result.Facts, graph.EdgeUsesInputAction,
			"godot:input_action:client/project.godot:"+action)
		if fact.Properties["form"] != "query" || fact.Properties["receiver"] != "InputEvent" {
			t.Fatalf("InputEvent action query fact = %#v", fact)
		}
		assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "input/"+action)
	}
}

// TestParserSourcesCrossFileConnectionsAtTheirHandler preserves the traversal
// added before named fact sources existed: the subscribes route remains sourced
// at the handler this script declares rather than at the statement that wired
// it, even though handled_by can now express the signal-to-handler direction.
func TestParserSourcesCrossFileConnectionsAtTheirHandler(t *testing.T) {
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/kit.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Kit extends Node

var _backend: Backend

func wire() -> void:
	_backend.sign_in_success.connect(_on_sign_in)
	_backend.sign_in_failed.connect(func(): pass)

func _on_sign_in() -> void:
	pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Kit._on_sign_in")
	routed := findFactWithTarget(t, result.Facts, graph.EdgeSubscribes, "Backend.sign_in_success")
	if routed.FromID != handler.ID {
		t.Fatalf("a resolved handler must source the route: %#v", routed)
	}
	if routed.Properties["handler"] != "Kit._on_sign_in" || routed.Properties["site"] != "Kit.wire" {
		t.Fatalf("the route lost its handler or its call site: %#v", routed)
	}
	// A lambda proves no method, so the route stays at the wiring statement.
	wire := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Kit.wire")
	lambda := findFactWithTarget(t, result.Facts, graph.EdgeSubscribes, "Backend.sign_in_failed")
	if lambda.FromID != wire.ID {
		t.Fatalf("an unprovable handler must leave the route at its call site: %#v", lambda)
	}
	if _, ok := lambda.Properties["handler"]; ok {
		t.Fatalf("a lambda proves no method; got %#v", lambda)
	}
}

// TestParserRefusesActionQueriesOnKnownNonInputReceivers pins the receiver rule
// and, just as importantly, its boundary. A receiver the type table knows to be
// something other than an input class is not a Godot input query, so nothing is
// emitted. A receiver whose type is unknown keeps emitting, because untyped
// parameters are pervasive in GDScript and refusing them would destroy most real
// recall - a deliberate decision this test exists to keep from being quietly
// tightened later.
func TestParserRefusesActionQueriesOnKnownNonInputReceivers(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "client/project.godot", "config_version=5\n")
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "client/scripts/pad.gd", Repository: "sample", RepoID: "repo:sample",
		Content: []byte(`class_name Pad extends Node

class LocalEvent extends InputEventKey:
	var tag: int

var _gamepad: Gamepad
var _local: LocalEvent
var helper: Node

func poll(typed: InputEvent, untyped) -> void:
	if _gamepad.is_action("refused_known"):
		pass
	if _gamepad.is_action_pressed("refused_family"):
		pass
	var refused_strength = _gamepad.get_action_strength("refused_strength")
	if typed.is_action("kept_typed"):
		pass
	if untyped.is_action("kept_untyped"):
		pass
	if _local.is_action("kept_local_subclass"):
		pass
	if self.helper.is_action_pressed("kept_self_field"):
		pass
`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"refused_known", "refused_family", "refused_strength"} {
		for _, fact := range result.Facts {
			if fact.Kind == graph.EdgeUsesInputAction && fact.Properties["action"] == action {
				t.Fatalf("a known non-input receiver must not produce an action edge: %#v", fact)
			}
			if fact.Kind == graph.EdgeReadsConfig && fact.Target == "input/"+action {
				t.Fatalf("a known non-input receiver must not read an input action: %#v", fact)
			}
		}
	}
	// Member-expression field typing is intentionally not shared with the HTTP
	// adapter. Keeping this receiver unknown preserves the established input
	// boundary for cross-file Input subclasses the single-file parser cannot see.
	for _, action := range []string{"kept_typed", "kept_untyped", "kept_local_subclass", "kept_self_field"} {
		findFactWithTarget(t, result.Facts, graph.EdgeUsesInputAction,
			"godot:input_action:client/project.godot:"+action)
		assertHasFact(t, result.Facts, graph.EdgeReadsConfig, "input/"+action)
	}
}

func TestParserFollowsGodotTreeShape(t *testing.T) {
	root := testtemp.Dir(t)
	writeFile(t, root, "project.godot", "[autoload]\nSession=\"*res://session.gd\"\n")
	content := []byte(`class_name Shape extends Node

enum Mode {
	IDLE,
	RUNNING = 4,
}

@export var speed: float = 10.0
@export
@onready var label: Label = $Label

@rpc("any_peer")
func sync() -> void:
	pass

func keys() -> Dictionary:
	return {Session = 1}
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Root: root, Path: "shape.gd", Content: content, Repository: "sample", RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	for qualified, want := range map[string]string{"Shape.speed": "export", "Shape.label": "export,onready"} {
		if got := findQualifiedNode(t, result.Nodes, graph.KindField, qualified).Properties["annotations"]; got != want {
			t.Fatalf("%s annotations = %q, want %q", qualified, got, want)
		}
	}
	if got := findQualifiedNode(t, result.Nodes, graph.KindMethod, "Shape.sync").Properties["annotations"]; got != "rpc" {
		t.Fatalf("Shape.sync annotations = %q, want rpc", got)
	}
	for qualified, line := range map[string]int{"Shape.Mode.IDLE": 4, "Shape.Mode.RUNNING": 5} {
		if got := findQualifiedNode(t, result.Nodes, graph.KindField, qualified).Location.Line; got != line {
			t.Fatalf("%s line = %d, want %d", qualified, got, line)
		}
	}
	for _, fact := range result.Facts {
		if fact.Properties["form"] == "autoload_reference" {
			t.Fatalf("Lua-style dictionary key produced an autoload reference: %#v", fact)
		}
	}
}

// TestParserRecordsClassConstructionFromNew proves ClassName.new(...) is
// recorded against the class it constructs, so a construction site is
// reachable from the type, and that a receiver bound to a value in scope keeps
// producing an ordinary call instead of a guessed construction edge.
func TestParserRecordsClassConstructionFromNew(t *testing.T) {
	content := []byte(`class_name InGameSessionCoordinator
extends Node

var _modules: Array = []

func _build_container_runtime(factory, typed: TradeModule) -> void:
	_modules.append(TradeModule.new(self))
	_modules.append(typed.new())
	_modules.append(factory.new())
	_modules.append(loader.new())
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "session/in_game_session_coordinator.gd", Content: content, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	construction := 0
	for _, fact := range result.Facts {
		if fact.Kind != graph.EdgeCalls || fact.Properties["form"] != "construction" {
			continue
		}
		construction++
		if fact.Target != "TradeModule" || fact.TargetKind != graph.KindClass ||
			fact.Properties["constructor"] != "new" || fact.Location.Line == 0 {
			t.Fatalf("construction fact lost its evidence: %#v", fact)
		}
	}
	// The bare class name and the parameter declared as that class are both
	// proof of construction; the untyped local and the unbound lowercase
	// receiver are not.
	if construction != 2 {
		t.Fatalf("construction facts = %d, want 2", construction)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "factory.new")
	assertHasFact(t, result.Facts, graph.EdgeCalls, "loader.new")
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeCalls && fact.Target == "TradeModule.new" {
			t.Fatalf("construction still spelled as a call to a new member: %#v", fact)
		}
	}
}

// TestParserRecordsMethodValuesHandedToCallsites proves a method named as a
// callable value is recorded against the declaration that names it, in every
// shape a value can take, and that a name which proves no method - a shadowing
// parameter, a nested call's callee, or a member of a foreign receiver - records
// nothing rather than a guessed reference.
func TestParserRecordsMethodValuesHandedToCallsites(t *testing.T) {
	content := []byte(`class_name InGameSessionCoordinator
extends Node

var _modules: Array = []

func _build_container_runtime(reconcile) -> void:
	_modules.append(ActionRejectionFeedbackModule.new(_reconcile_rejected_request))
	_install(self._world_feedback_has_world)
	_install(reconcile)
	_install(_world_feedback_has_world())
	_install(other._reconcile_rejected_request)

func _install(_handler) -> void:
	pass

func _reconcile_rejected_request(_request_id: int, _reason: String) -> void:
	pass

func _world_feedback_has_world() -> bool:
	return true
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "session/in_game_session_coordinator.gd", Content: content, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]graph.Fact{}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeReferences && fact.Properties["form"] == "method_value" {
			values[fact.Target] = fact
		}
	}
	for _, expected := range []struct {
		target, passedTo, argument string
	}{
		{"InGameSessionCoordinator._reconcile_rejected_request", "ActionRejectionFeedbackModule.new", "0"},
		{"InGameSessionCoordinator._world_feedback_has_world", "InGameSessionCoordinator._install", "0"},
	} {
		fact, ok := values[expected.target]
		if !ok {
			t.Fatalf("missing method value reference to %q: %#v", expected.target, values)
		}
		if fact.TargetKind != graph.KindMethod || fact.Properties["passed_to"] != expected.passedTo ||
			fact.Properties["argument"] != expected.argument || fact.Location.Line == 0 {
			t.Fatalf("method value reference lost its evidence: %#v", fact)
		}
	}
	// A shadowing parameter, a nested call's callee, and a member of a receiver
	// this file does not declare each prove no method value.
	if len(values) != 2 {
		t.Fatalf("method value references = %#v", values)
	}
}

// TestParserRecordsCallableInvocationAgainstItsDeclaration proves a call
// through a stored Callable is recorded against the declaration that holds it,
// that the declaration keeps every assignment into it as explicit evidence
// rather than resolving to a guessed method, and that an untyped receiver keeps
// producing an ordinary call.
func TestParserRecordsCallableInvocationAgainstItsDeclaration(t *testing.T) {
	content := []byte(`class_name ActionRejectionFeedbackModule
extends Node

var _reconcile_rejected_request: Callable
var _dispatch

func _init(reconcile_rejected_request: Callable) -> void:
	_reconcile_rejected_request = reconcile_rejected_request

func _on_action_rejected(request_id: int, reason: String) -> void:
	_reconcile_rejected_request.call(request_id, reason)
	self._reconcile_rejected_request.call_deferred(request_id)
	_dispatch.call(request_id)

func _rebind() -> void:
	_reconcile_rejected_request = _local_reconcile
	_reconcile_rejected_request = _local_report

func _local_reconcile(_request_id: int, _reason: String) -> void:
	pass

func _local_report(_request_id: int, _reason: String) -> void:
	pass
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "modules/action_rejection_feedback_module.gd", Content: content, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	field := gdNodeNamed(t, result.Nodes, "_reconcile_rejected_request")
	invocations := map[string]bool{}
	for _, fact := range result.Facts {
		if fact.Kind != graph.EdgeCalls || fact.Properties["form"] != "callable_invocation" {
			continue
		}
		if fact.TargetID != field.ID || fact.Target != "" {
			t.Fatalf("callable invocation did not name the stored declaration: %#v", fact)
		}
		invocations[fact.Properties["invoke"]] = true
	}
	// Both the bare and the self spelling of the field invoke the same stored
	// declaration; an untyped receiver proves nothing and stays an ordinary call.
	if len(invocations) != 2 || !invocations["call"] || !invocations["call_deferred"] {
		t.Fatalf("callable invocations = %#v", invocations)
	}
	assertHasFact(t, result.Facts, graph.EdgeCalls, "_dispatch.call")

	// A field reassigned from two methods keeps both as explicit evidence, so
	// no invocation resolves to a guessed one of them.
	rebound := map[string]bool{}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeReferences && fact.Properties["form"] == "method_value" {
			rebound[fact.Target] = true
		}
	}
	for _, method := range []string{
		"ActionRejectionFeedbackModule._local_reconcile",
		"ActionRejectionFeedbackModule._local_report",
	} {
		if !rebound[method] {
			t.Fatalf("missing method value reference to %q: %#v", method, rebound)
		}
	}
	// The constructor parameter reaches the field it is stored into.
	parameter := gdNodeNamed(t, result.Nodes, "reconcile_rejected_request")
	stored := false
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeAssigns && fact.FromID == parameter.ID && fact.TargetID == field.ID {
			stored = true
		}
	}
	if !stored {
		t.Fatalf("constructor parameter does not reach the field it is stored into: %#v", result.Facts)
	}
}

// TestParserRefusesCallableInvocationWithoutProvenDeclaration proves the
// Callable invocation rule needs a declaration it can see: a malformed or
// absent annotation, an inferred binding, and a receiver declared elsewhere
// each keep producing an ordinary call rather than a guessed invocation edge.
func TestParserRefusesCallableInvocationWithoutProvenDeclaration(t *testing.T) {
	for _, testCase := range []struct {
		name, source, callee string
	}{
		{"untyped field", "var _handler\n", "_handler.call"},
		{"inferred from a method value", "var _handler = _target\n", "_handler.call"},
		{"foreign type", "var _handler: Node\n", "Node.call"},
		{"lowercase lookalike type", "var _handler: callable\n", "callable.call"},
		{"undeclared name", "", "_handler.call"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			content := []byte("class_name Holder\nextends Node\n\n" + testCase.source + `
func run() -> void:
	_handler.call(1)

func _target(_value: int) -> void:
	pass
`)
			result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
				Path: "modules/holder.gd", Content: content, RepoID: "repo:sample",
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, fact := range result.Facts {
				if fact.Properties["form"] == "callable_invocation" {
					t.Fatalf("unproven receiver produced a callable invocation: %#v", fact)
				}
			}
			assertHasFact(t, result.Facts, graph.EdgeCalls, testCase.callee)
		})
	}
}

// TestParserRefusesShadowedSignalHandlerName proves a connect argument whose
// name is shadowed by a parameter routes the signal to nothing: the value is
// whatever the caller supplied, so naming the method it is spelled like would
// be a guessed edge.
func TestParserRefusesShadowedSignalHandlerName(t *testing.T) {
	content := []byte(`class_name Player
extends Node

signal finished(value: int)

func wire(on_finished: Callable) -> void:
	finished.connect(on_finished)

func rewire() -> void:
	finished.connect(on_finished)

func on_finished(_value: int) -> void:
	pass
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "scripts/player.gd", Content: content, RepoID: "repo:sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	handlers := 0
	for _, fact := range result.Facts {
		if fact.Kind != graph.EdgeHandledBy {
			continue
		}
		handlers++
		if fact.Target != "Player.on_finished" || fact.Location.Line != 10 {
			t.Fatalf("signal handler lost its evidence: %#v", fact)
		}
	}
	// Only the unshadowed spelling names a method; the parameter holds a value
	// this file cannot resolve to one.
	if handlers != 1 {
		t.Fatalf("signal handler facts = %d, want 1", handlers)
	}
}

func TestParserResolvesCallsThroughADeclaredReturnTypeAndRefusesWithoutOne(t *testing.T) {
	content := []byte(`extends GutTest

class Fixture:
	func reset() -> void:
		pass

func _make() -> Coordinator:
	return Coordinator.new()

func _make_fixture() -> Fixture:
	return Fixture.new()

func _make_untyped():
	return Coordinator.new()

func test_inferred_local() -> void:
	var made := _make()
	made.handle()

func test_inferred_through_self() -> void:
	var made := self._make()
	made.handle()

func test_inferred_inner_class() -> void:
	var fixture := _make_fixture()
	fixture.reset()

func test_unannotated_return_resolves_nothing() -> void:
	var made := _make_untyped()
	made.handle()

func test_shadowed_name_resolves_nothing() -> void:
	var _make := func(): return Coordinator.new()
	var made := _make.call()
	made.handle()
`)
	result, err := gdscriptparser.New().Parse(context.Background(), parserapi.Input{
		Path: "tests/coordinator_test.gd", Content: content, Repository: "sample", RepoID: "repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]bool{}
	for _, fact := range result.Facts {
		if fact.Kind == graph.EdgeCalls {
			targets[fact.Target] = true
		}
	}
	for _, want := range []string{"Coordinator.handle", "tests/coordinator_test.Fixture.reset"} {
		if !targets[want] {
			t.Errorf("missing resolved call to %s; calls = %v", want, sortedKeys(targets))
		}
	}
	for _, refused := range []string{"made.handle"} {
		if !targets[refused] {
			t.Errorf("expected the unresolved receiver %s to stay unresolved; calls = %v",
				refused, sortedKeys(targets))
		}
	}
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
