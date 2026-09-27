package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBatchInputsNormalizesScalarAndPluralForms(t *testing.T) {
	many := make([]string, maxBatchInputs+1)
	for index := range many {
		many[index] = "selector"
	}
	tests := []struct {
		name        string
		scalar      string
		plural      []string
		want        []string
		wantBatched bool
		wantError   string
	}{
		{name: "scalar only", scalar: "pkg.Fn", want: []string{"pkg.Fn"}},
		{name: "scalar is trimmed", scalar: "  pkg.Fn  ", want: []string{"pkg.Fn"}},
		{name: "plural only", plural: []string{"a", "b"}, want: []string{"a", "b"}, wantBatched: true},
		{name: "plural drops blanks", plural: []string{"a", "   ", "b"}, want: []string{"a", "b"}, wantBatched: true},
		{name: "plural preserves caller order", plural: []string{"b", "a"}, want: []string{"b", "a"}, wantBatched: true},
		{name: "agreeing forms stay scalar", scalar: "a", plural: []string{"a"}, want: []string{"a"}},
		{name: "neither form", wantError: "selector or selectors is required"},
		{name: "all blank", scalar: "  ", plural: []string{" "}, wantError: "selector or selectors is required"},
		{name: "disagreeing forms", scalar: "a", plural: []string{"b"}, wantError: "disagree"},
		{name: "scalar with longer plural", scalar: "a", plural: []string{"a", "b"}, wantError: "disagree"},
		{name: "over the bound", plural: many, wantError: "at most"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, batched, err := batchInputs("selector", test.scalar, test.plural)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("want error containing %q, got %v", test.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if batched != test.wantBatched {
				t.Fatalf("batched = %v, want %v", batched, test.wantBatched)
			}
			if strings.Join(got, ",") != strings.Join(test.want, ",") {
				t.Fatalf("inputs = %v, want %v", got, test.want)
			}
		})
	}
}

func TestPathInputsNormalizesScalarAndPluralForms(t *testing.T) {
	many := make([]PathPair, maxBatchInputs+1)
	for index := range many {
		many[index] = PathPair{From: "a", To: "b"}
	}
	tests := []struct {
		name        string
		input       PathInput
		want        []PathPair
		wantBatched bool
		wantError   string
	}{
		{name: "scalar pair", input: PathInput{From: "a", To: "b"}, want: []PathPair{{From: "a", To: "b"}}},
		{
			name: "plural pairs", input: PathInput{Pairs: []PathPair{{From: "a", To: "b"}, {From: "c", To: "d"}}},
			want: []PathPair{{From: "a", To: "b"}, {From: "c", To: "d"}}, wantBatched: true,
		},
		{
			name: "agreeing forms stay scalar", input: PathInput{From: "a", To: "b", Pairs: []PathPair{{From: "a", To: "b"}}},
			want: []PathPair{{From: "a", To: "b"}},
		},
		{name: "half a scalar pair", input: PathInput{From: "a"}, wantError: "must be supplied together"},
		{name: "neither form", input: PathInput{}, wantError: "is required"},
		{name: "incomplete plural pair", input: PathInput{Pairs: []PathPair{{From: "a"}}}, wantError: "pairs[0]"},
		{
			name: "disagreeing forms", input: PathInput{From: "a", To: "b", Pairs: []PathPair{{From: "c", To: "d"}}},
			wantError: "disagree",
		},
		{name: "over the bound", input: PathInput{Pairs: many}, wantError: "at most"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, batched, err := pathInputs(test.input)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("want error containing %q, got %v", test.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if batched != test.wantBatched {
				t.Fatalf("batched = %v, want %v", batched, test.wantBatched)
			}
			if len(got) != len(test.want) {
				t.Fatalf("pairs = %v, want %v", got, test.want)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Fatalf("pairs[%d] = %v, want %v", index, got[index], test.want[index])
				}
			}
		})
	}
}

func TestRunBatchKeepsOrderAndIsolatesFailures(t *testing.T) {
	results := runBatch(context.Background(), []string{"ok", "bad", "also-ok"},
		func(_ context.Context, input string) (string, error) {
			if input == "bad" {
				return "", errors.New("boom")
			}
			return strings.ToUpper(input), nil
		})
	if len(results) != 3 {
		t.Fatalf("expected one envelope per input, got %d", len(results))
	}
	for index, envelope := range results {
		if envelope.Index != index {
			t.Fatalf("envelope %d reports index %d", index, envelope.Index)
		}
	}
	if results[0].Value == nil || *results[0].Value != "OK" || results[0].Error != "" {
		t.Fatalf("unexpected first envelope: %#v", results[0])
	}
	if results[1].Value != nil || results[1].Error != "boom" {
		t.Fatalf("a failed input must report only its own error: %#v", results[1])
	}
	if results[2].Value == nil || *results[2].Value != "ALSO-OK" {
		t.Fatalf("a failed input must not erase later successes: %#v", results[2])
	}
}

func TestRunBatchReportsCancelledInputsWithoutRunningThem(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	results := runBatch(ctx, []string{"a", "b"}, func(_ context.Context, input string) (string, error) {
		calls++
		return input, nil
	})
	if calls != 0 {
		t.Fatalf("expected no work after cancellation, got %d calls", calls)
	}
	for _, envelope := range results {
		if envelope.Error == "" {
			t.Fatalf("expected a cancellation error for every input: %#v", envelope)
		}
	}
}

func TestFirstValueSurfacesScalarErrors(t *testing.T) {
	failure := []ResultEnvelope[string]{{Index: 0, Input: "a", Error: "boom"}}
	if _, err := firstValue(failure, false); err == nil || err.Error() != "boom" {
		t.Fatalf("a scalar request must fail outright, got %v", err)
	}
	if _, err := firstValue(failure, true); err != nil {
		t.Fatalf("a batched request must not fail on one bad input, got %v", err)
	}
	value := "found"
	success := []ResultEnvelope[string]{{Index: 0, Input: "a", Value: &value}}
	got, err := firstValue(success, false)
	if err != nil || got != "found" {
		t.Fatalf("firstValue = (%q, %v)", got, err)
	}
	if got, err := firstValue(success, true); err != nil || got != "" {
		t.Fatalf("a batched request must leave the legacy field empty, got (%q, %v)", got, err)
	}
}
