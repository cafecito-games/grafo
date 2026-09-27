package typescript

import (
	"encoding/json"
	"testing"
)

func TestStripJSONCommentsPreservesCommaDelimitersInsideStrings(t *testing.T) {
	content := []byte(`{
		// a comment
		"patterns": ["a,]b", "c,}d",],
		"nested": {"enabled": true,},
	}`)
	var decoded struct {
		Patterns []string `json:"patterns"`
		Nested   struct {
			Enabled bool `json:"enabled"`
		} `json:"nested"`
	}
	if err := json.Unmarshal(stripJSONComments(content), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Patterns) != 2 || decoded.Patterns[0] != "a,]b" || decoded.Patterns[1] != "c,}d" || !decoded.Nested.Enabled {
		t.Fatalf("unexpected decoded JSONC: %#v", decoded)
	}
}

func TestScanModuleUsesOuterAnonymousDefaultDeclaration(t *testing.T) {
	content := []byte("export default class extends mixin(\n  class {}\n) { run() {} }")
	info, err := scanModule("nested.ts", content)
	if err != nil {
		t.Fatal(err)
	}
	refs := info.exports["default"]
	if len(refs) != 1 || refs[0].local != "anonymous@1" {
		t.Fatalf("unexpected default export refs: %#v", refs)
	}
	if _, ok := info.locals[refs[0].local]; !ok {
		t.Fatalf("default export does not resolve to an outer local: %#v", info.locals)
	}
}
