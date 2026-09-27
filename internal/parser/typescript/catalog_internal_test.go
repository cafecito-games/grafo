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
