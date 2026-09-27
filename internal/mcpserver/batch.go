package mcpserver

import (
	"context"
	"fmt"
	"strings"
)

// maxBatchInputs bounds one batched tool call. Batching exists to remove
// per-symbol round trips, not to let a single request walk the whole graph.
const maxBatchInputs = 50

// ResultEnvelope carries the outcome of one batched input. Results keep the
// caller's order, and a failed input reports its own error without erasing
// unrelated successful results.
type ResultEnvelope[T any] struct {
	Index int    `json:"index"`
	Input string `json:"input,omitempty"`
	Value *T     `json:"value,omitempty"`
	Error string `json:"error,omitempty"`
}

// batchInputs normalizes a tool's scalar and plural input forms into one
// ordered list. The scalar field remains supported for one release. Supplying
// both forms is accepted only when they describe the same single input;
// anything else is rejected rather than silently preferring one form.
// The returned batched flag reports whether the caller used only the plural
// form, which is what decides between the legacy scalar response shape and the
// per-input envelope list.
func batchInputs(name, scalar string, plural []string) ([]string, bool, error) {
	scalar = strings.TrimSpace(scalar)
	cleaned := make([]string, 0, len(plural))
	for _, value := range plural {
		if value = strings.TrimSpace(value); value != "" {
			cleaned = append(cleaned, value)
		}
	}
	var inputs []string
	batched := scalar == "" && len(cleaned) > 0
	switch {
	case scalar == "" && len(cleaned) == 0:
		return nil, false, fmt.Errorf("%s or %ss is required", name, name)
	case len(cleaned) == 0:
		inputs = []string{scalar}
	case scalar == "":
		inputs = cleaned
	case len(cleaned) == 1 && cleaned[0] == scalar:
		inputs = cleaned
	default:
		return nil, false, fmt.Errorf("%s and %ss disagree; supply only one form", name, name)
	}
	if len(inputs) > maxBatchInputs {
		return nil, false, fmt.Errorf("%ss accepts at most %d inputs, got %d", name, maxBatchInputs, len(inputs))
	}
	return inputs, batched, nil
}

// runBatch applies run to every input in order and records one envelope per
// input. Cancellation stops further work but still reports the remaining
// inputs so the caller can tell what was not attempted.
func runBatch[T any](ctx context.Context, inputs []string, run func(context.Context, string) (T, error)) []ResultEnvelope[T] {
	results := make([]ResultEnvelope[T], 0, len(inputs))
	for index, input := range inputs {
		envelope := ResultEnvelope[T]{Index: index, Input: input}
		if err := ctx.Err(); err != nil {
			envelope.Error = err.Error()
			results = append(results, envelope)
			continue
		}
		value, err := run(ctx, input)
		if err != nil {
			envelope.Error = err.Error()
		} else {
			envelope.Value = &value
		}
		results = append(results, envelope)
	}
	return results
}

// firstValue returns the single successful value of a scalar-shaped request so
// legacy clients keep reading the original top-level fields. It also surfaces
// the error for a one-input request, where failing the call is more useful
// than returning an envelope the caller never asked for.
func firstValue[T any](results []ResultEnvelope[T], batched bool) (T, error) {
	var zero T
	if batched || len(results) != 1 {
		return zero, nil
	}
	if results[0].Error != "" {
		return zero, fmt.Errorf("%s", results[0].Error)
	}
	if results[0].Value == nil {
		return zero, nil
	}
	return *results[0].Value, nil
}
