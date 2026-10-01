package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/testtemp"
)

// TestRunReportsInterruptionInsteadOfTheWrappedCause covers the generic
// failure branch. A signalled run surfaces whatever the innermost dependency
// was doing, wrapped on the way out and carrying a cancel cause that does not
// unwrap to context.Canceled, so only the context says the run was
// interrupted. Rendering the cause blamed an embedding provider or an HTTP
// endpoint for an interruption neither of them caused.
func TestRunReportsInterruptionInsteadOfTheWrappedCause(t *testing.T) {
	root := testtemp.Dir(t)
	for _, testCase := range []struct {
		name        string
		interrupted bool
		want        string
		unwanted    string
	}{
		{name: "interrupted", interrupted: true, want: "grafo: interrupted\n", unwanted: "has no index"},
		{name: "live", want: "has no index", unwanted: "interrupted"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase.interrupted {
				cancel()
			}
			var stdout, stderr bytes.Buffer
			code := New(&stdout, &stderr).Run(ctx, []string{"find", "Charge", "--repo", root})
			if code != 1 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), testCase.want) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), testCase.want)
			}
			if strings.Contains(stderr.String(), testCase.unwanted) {
				t.Fatalf("stderr = %q, want it to omit %q", stderr.String(), testCase.unwanted)
			}
		})
	}
}

// TestRunKeepsTheFailureExitStatusForInterruptions pins the deliberate
// decision that only the rendered text changes: an interrupted run is still a
// run that produced no answer, and callers already branch on a nonzero status.
func TestRunKeepsTheFailureExitStatusForInterruptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := New(&stdout, &stderr).Run(ctx, []string{"find", "Charge", "--repo", testtemp.Dir(t)}); code != 1 {
		t.Fatalf("interrupted exit code = %d, want 1", code)
	}
}

// TestRunCanceledConsultsTheContextForOpaqueCauses pins why the context is
// consulted: os/signal cancels with a cause that reports the signal, and that
// cause does not unwrap to context.Canceled.
func TestRunCanceledConsultsTheContextForOpaqueCauses(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("interrupt signal received"))
	wrapped := fmt.Errorf("embed candidates: call Ollama at http://localhost:11434: %w", context.Cause(ctx))
	if errors.Is(wrapped, context.Canceled) {
		t.Fatal("a signal cause now unwraps to context.Canceled; the context check is redundant")
	}
	if !runCanceled(ctx, wrapped) {
		t.Fatal("runCanceled missed an interrupted run carrying an opaque cause")
	}
	live, stop := context.WithTimeout(context.Background(), time.Minute)
	defer stop()
	if runCanceled(live, errors.New("ollama is unreachable")) {
		t.Fatal("runCanceled treated a genuine provider failure as an interruption")
	}
}
