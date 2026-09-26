package sql

import (
	"strings"
	"testing"
)

func TestConfigurationPathMappingsAreSpecificAndOrderIndependent(t *testing.T) {
	config, err := parseConfiguration([]byte(`other:
  value: ignored
sql:
  paths:
    "db/**": sqlite
    "db/migrations/*.sql": postgres
`))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]string{
		"db/schema.sql":              "sqlite",
		"db/migrations/001_init.sql": "postgres",
		"other/schema.sql":           "",
	}
	for filePath, want := range tests {
		got, err := config.dialectForPath(filePath)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("dialectForPath(%q) = %q, want %q", filePath, got, want)
		}
	}
}

func TestConfigurationRejectsConflictingDuplicatePattern(t *testing.T) {
	_, err := parseConfiguration([]byte(`sql:
  paths:
    "db/**": sqlite
    "db/**": postgres
`))
	if err == nil || !strings.Contains(err.Error(), "maps to both") {
		t.Fatalf("expected duplicate mapping error, got %v", err)
	}
}
