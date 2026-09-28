package agentguide

import (
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/projectconfig"
	"gopkg.in/yaml.v3"
)

func TestTextCoversRequiredRouting(t *testing.T) {
	text := Text()
	for _, fragment := range []string{
		"get_index_status", "find_symbols", "get_callers", "get_callees",
		"find_path", "get_neighbors", "get_blast_radius", "find_reusable_code",
		"get_message_flow", "list_message_coverage",
		"upstream", "downstream", "truncated", "native", "grafo index .",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("canonical guidance is missing %q", fragment)
		}
	}
	if !strings.Contains(text, versionComment()) {
		t.Error("canonical guidance is missing its version marker")
	}
}

func TestSkillCarriesFrontmatter(t *testing.T) {
	skill := Skill()
	if !strings.HasPrefix(skill, "---\n") {
		t.Fatalf("skill does not start with frontmatter: %q", skill[:min(40, len(skill))])
	}
	if !strings.Contains(skill, "name: grafo\n") {
		t.Error("skill frontmatter is missing the skill name")
	}
	if !strings.Contains(skill, Text()) {
		t.Error("skill does not embed the canonical guidance")
	}
	if !strings.HasSuffix(skill, "\n") {
		t.Error("skill does not end with a newline")
	}
}

func TestSetupSkillCoversRepositoryOnboarding(t *testing.T) {
	skill := SetupSkill()
	for _, fragment := range []string{
		"name: grafo-setup", "grafo.yaml", "components:", "adapters:",
		"default_dialect:", "gdscript_bases:", "message-coverage", "message-flow",
		"event.publish", "event.subscribe", "event.unsubscribe", "event.connection_test",
		"http.request", "the `event` role", "`method` and `url`", "exact qualified symbol",
		"baseline", "reindex",
		"name, comment, or popularity is\n  never proof", "do not use wildcards",
		"legacy `http.request_apis` compatibility alias", "same symbol through both forms",
		"do not add transport or serialization keys", "not a wildcard plugin mechanism",
		setupVersionComment(),
	} {
		if !strings.Contains(skill, fragment) {
			t.Errorf("setup skill is missing %q", fragment)
		}
	}
	if !OwnsSetup(skill) {
		t.Fatal("setup skill does not carry its ownership marker")
	}
	if OwnsSetup(Skill()) {
		t.Fatal("structural guidance claims setup-skill ownership")
	}
	if !strings.HasSuffix(skill, "\n") {
		t.Fatal("setup skill does not end with a newline")
	}
}

func TestStructuralGuidanceTreatsAdaptersAsAnUncertaintyBoundary(t *testing.T) {
	text := Text()
	for _, fragment := range []string{
		"repository abstraction", "grafo.yaml", "adapters", "native source inspection",
		"http.request_apis", "read-only", "grafo-setup", "reindex",
	} {
		if !strings.Contains(text, fragment) {
			t.Errorf("structural guidance is missing %q", fragment)
		}
	}
}

func TestGuidanceVersionsAndMarkersAreIndependent(t *testing.T) {
	if Version != "4" {
		t.Fatalf("Version = %q, want 4", Version)
	}
	if SetupVersion != "2" {
		t.Fatalf("SetupVersion = %q, want 2", SetupVersion)
	}
	if !strings.Contains(Skill(), versionComment()) || strings.Contains(Skill(), setupVersionComment()) {
		t.Fatal("structural skill does not carry only its own version marker")
	}
	if !strings.Contains(SetupSkill(), setupVersionComment()) || strings.Contains(SetupSkill(), versionComment()) {
		t.Fatal("setup skill does not carry only its own version marker")
	}
}

func TestSetupAdapterExampleMatchesProjectConfiguration(t *testing.T) {
	skill := SetupSkill()
	const begin = "<!-- BEGIN adapter-example -->\n```yaml\n"
	const end = "\n```\n<!-- END adapter-example -->"
	_, after, ok := strings.Cut(skill, begin)
	if !ok {
		t.Fatal("setup skill is missing the adapter example marker")
	}
	example, _, ok := strings.Cut(after, end)
	if !ok {
		t.Fatal("setup skill has an unterminated adapter example")
	}
	config, err := projectconfig.Parse([]byte(example + "\n"))
	if err != nil {
		t.Fatalf("documented adapter example does not parse: %v\n%s", err, example)
	}
	if got := len(config.Adapters.Lookup("gdscript", "Signals.wire")); got != 1 {
		t.Fatalf("documented event adapter effects = %d, want 1", got)
	}
	if got := len(config.Adapters.Lookup("gdscript", "API.fetch")); got != 1 {
		t.Fatalf("documented HTTP adapter effects = %d, want 1", got)
	}
}

func TestBlockIsDelimitedAndVersioned(t *testing.T) {
	block := Block()
	if !strings.HasPrefix(block, BeginMarker+"\n") || !strings.HasSuffix(block, EndMarker+"\n") {
		t.Fatalf("block is not delimited by the Grafo markers: %q", block)
	}
	if strings.Count(block, BeginMarker) != 1 || strings.Count(block, EndMarker) != 1 {
		t.Fatal("block repeats its markers")
	}
	if !strings.Contains(block, Text()) {
		t.Error("block does not embed the canonical guidance")
	}
}

func TestUpsertBlockAppendsAndPreservesUserContent(t *testing.T) {
	existing := "# My rules\n\nAlways run the tests.\n"
	merged, change, err := UpsertBlock(existing)
	if err != nil {
		t.Fatal(err)
	}
	if change != Added {
		t.Fatalf("change = %v, want %v", change, Added)
	}
	if !strings.HasPrefix(merged, existing) {
		t.Fatalf("user content was not preserved verbatim: %q", merged)
	}
	if !strings.Contains(merged, Block()) {
		t.Fatal("managed block was not appended")
	}

	again, change, err := UpsertBlock(merged)
	if err != nil {
		t.Fatal(err)
	}
	if change != Unchanged {
		t.Fatalf("re-running produced %v, want %v", change, Unchanged)
	}
	if again != merged {
		t.Fatal("re-running rewrote an already current block")
	}
}

func TestUpsertBlockReplacesOnlyOwnedContent(t *testing.T) {
	stale := "before\n" + BeginMarker + "\nold grafo text\n" + EndMarker + "\nafter\n"
	merged, change, err := UpsertBlock(stale)
	if err != nil {
		t.Fatal(err)
	}
	if change != Updated {
		t.Fatalf("change = %v, want %v", change, Updated)
	}
	if !strings.HasPrefix(merged, "before\n") || !strings.HasSuffix(merged, "after\n") {
		t.Fatalf("surrounding user content changed: %q", merged)
	}
	if strings.Contains(merged, "old grafo text") {
		t.Error("stale Grafo content survived the refresh")
	}
	if strings.Count(merged, BeginMarker) != 1 {
		t.Error("refresh duplicated the managed block")
	}
}

func TestUpsertBlockRefusesConflictingMarkers(t *testing.T) {
	cases := map[string]string{
		"duplicated": "a\n" + Block() + "b\n" + Block(),
		"begin only": "a\n" + BeginMarker + "\ntext\n",
		"end only":   "a\n" + EndMarker + "\ntext\n",
		"inverted":   "a\n" + EndMarker + "\ntext\n" + BeginMarker + "\n",
	}
	for name, existing := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := UpsertBlock(existing); err == nil {
				t.Fatal("expected a conflict error")
			}
			if _, _, err := RemoveBlock(existing); err == nil {
				t.Fatal("expected a conflict error from RemoveBlock")
			}
		})
	}
}

func TestRemoveBlockLeavesUnrelatedContentByteEqual(t *testing.T) {
	existing := "# My rules\n\nAlways run the tests.\n"
	merged, _, err := UpsertBlock(existing)
	if err != nil {
		t.Fatal(err)
	}
	removed, change, err := RemoveBlock(merged)
	if err != nil {
		t.Fatal(err)
	}
	if change != Removed {
		t.Fatalf("change = %v, want %v", change, Removed)
	}
	if removed != existing {
		t.Fatalf("remove did not restore the original bytes: %q, want %q", removed, existing)
	}
	again, change, err := RemoveBlock(removed)
	if err != nil {
		t.Fatal(err)
	}
	if change != Unchanged || again != removed {
		t.Fatalf("RemoveBlock is not idempotent: change=%v", change)
	}
}

func TestDigestIsStableAndPrefixed(t *testing.T) {
	first := Digest("grafo")
	if first != Digest("grafo") {
		t.Fatal("Digest is not deterministic")
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("digest %q is not labelled", first)
	}
	if first == Digest("grafo ") {
		t.Fatal("Digest ignores content")
	}
}

// TestSkillFrontmatterIsValidYAML parses the generated frontmatter instead of
// asserting on raw text: the description contains ": ", which is not a legal
// YAML plain scalar, so it must be quoted for a client to load the skill.
func TestSkillFrontmatterIsValidYAML(t *testing.T) {
	assertSkillFrontmatter(t, Skill(), Name, Description)
	assertSkillFrontmatter(t, SetupSkill(), SetupName, SetupDescription)
}

func assertSkillFrontmatter(t *testing.T, skill, wantName, wantDescription string) {
	t.Helper()
	rest, ok := strings.CutPrefix(skill, "---\n")
	if !ok {
		t.Fatal("skill has no frontmatter")
	}
	frontmatter, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("skill frontmatter is not terminated")
	}
	var fields struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(frontmatter+"\n"), &fields); err != nil {
		t.Fatalf("frontmatter is not valid YAML: %v\n%s", err, frontmatter)
	}
	if fields.Name != wantName {
		t.Errorf("name = %q, want %q", fields.Name, wantName)
	}
	if fields.Description != wantDescription {
		t.Errorf("description = %q, want %q", fields.Description, wantDescription)
	}
}
