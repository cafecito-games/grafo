package graph

import "testing"

func TestFactValidatesExactlyOneSourceLocator(t *testing.T) {
	tests := []struct {
		name    string
		fact    Fact
		wantErr bool
	}{
		{name: "exact", fact: Fact{FromID: "node"}},
		{name: "named", fact: Fact{Source: "Backend.ready"}},
		{name: "named with kind", fact: Fact{Source: "Backend.ready", SourceKind: KindEvent}},
		{name: "missing", fact: Fact{}, wantErr: true},
		{name: "both", fact: Fact{FromID: "node", Source: "Backend.ready"}, wantErr: true},
		{name: "kind without name", fact: Fact{FromID: "node", SourceKind: KindEvent}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.fact.ValidateSourceLocator()
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateSourceLocator() error = %v, want error %v", err, test.wantErr)
			}
		})
	}
}
