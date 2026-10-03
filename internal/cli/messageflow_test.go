package cli_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/query"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestMessageFlowCommandsShareStructuredCoverageSemantics(t *testing.T) {
	t.Parallel()
	root := copyMessageFlowFixture(t)
	run(t, "index", root)

	var flow query.MessageFlow
	runJSON(t, &flow, "message-flow", "acme.v1.Envelope", "--repo", root, "--json")
	// Four sends carry the envelope: two GDScript ENetPacketPeer.send calls, one
	// through goenet's concrete Peer.Send, and one through a consumer interface
	// embedding goenet.PeerSender.
	if flow.Message.QualifiedName != "acme.v1.Envelope" || len(flow.Sends) != 4 || len(flow.Members) != 4 {
		t.Fatalf("message-flow JSON lost structured evidence: %#v", flow)
	}
	if output := run(t, "message-flow", "acme.v1.Envelope", "--repo", root); !strings.Contains(output, "acme.v1.Envelope") || !strings.Contains(output, "channel=3") {
		t.Fatalf("message-flow human output omitted canonical or transport evidence: %s", output)
	}

	var coverage query.MessageCoverageList
	runJSON(t, &coverage, "message-coverage", "--repo", root, "--package", "acme.v1", "--oneof", "payload",
		"--status", "unknown", "--path-prefix", "proto", "--json")
	if len(coverage.Messages) != 1 || len(coverage.Messages[0].Members) != 4 || coverage.Messages[0].Status != query.CoverageUnknown {
		t.Fatalf("message coverage filters changed service semantics: %#v", coverage)
	}
	if code, _, stderr := execute(t, "message-coverage", "--repo", root, "--status", "maybe"); code == 0 || !strings.Contains(stderr, "unknown message coverage status") {
		t.Fatalf("invalid coverage status was accepted: code=%d stderr=%s", code, stderr)
	}
	if code, _, stderr := execute(t, "message-flow", "acme.v1.Envelope", "--repo", root, "--path-prefix", "proto"); code == 0 {
		t.Fatalf("a path prefix was accepted by scalar message flow: %s", stderr)
	}
}

func copyMessageFlowFixture(t *testing.T) string {
	t.Helper()
	source, err := filepath.Abs(filepath.Join("..", "eval", "testdata", "enet-transport", "repos", "transport"))
	if err != nil {
		t.Fatal(err)
	}
	destination := testtemp.Dir(t)
	if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	return destination
}
