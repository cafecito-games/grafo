package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProgressSchemaV1 identifies the stable, source-free indexing progress schema.
const ProgressSchemaV1 = "grafo.index-progress/v1"

type ProgressPhase string

const (
	ProgressGitProbe       ProgressPhase = "git_probe"
	ProgressMembership     ProgressPhase = "membership"
	ProgressChangeProbe    ProgressPhase = "change_probe"
	ProgressDiscovery      ProgressPhase = "discovery"
	ProgressReadHash       ProgressPhase = "read_hash"
	ProgressParse          ProgressPhase = "parse"
	ProgressPersistence    ProgressPhase = "persistence"
	ProgressReconciliation ProgressPhase = "reconciliation"
	ProgressComplete       ProgressPhase = "complete"
)

type ProgressState string

const (
	ProgressStarted   ProgressState = "started"
	ProgressProgress  ProgressState = "progress"
	ProgressCompleted ProgressState = "completed"
	ProgressError     ProgressState = "error"
	ProgressCanceled  ProgressState = "canceled"
)

// ProgressEvent intentionally contains aggregate work only. Paths and source
// content never belong in progress output.
type ProgressEvent struct {
	Schema         string        `json:"schema"`
	RepositoryID   string        `json:"repository_id"`
	RepositoryName string        `json:"repository_name"`
	Branch         string        `json:"branch"`
	Phase          ProgressPhase `json:"phase"`
	State          ProgressState `json:"state"`
	Unit           string        `json:"unit,omitempty"`
	Completed      int           `json:"completed,omitempty"`
	Total          int           `json:"total,omitempty"`
	ElapsedMS      int64         `json:"elapsed_ms"`
	RebuildReason  string        `json:"rebuild_reason,omitempty"`
	Error          string        `json:"error,omitempty"`
}

type ProgressObserver func(ProgressEvent) error

type progressEmitter struct {
	observer ProgressObserver
	project  Project
	started  time.Time
	rebuild  string
}

func newProgressEmitter(project Project, observer ProgressObserver, started time.Time) *progressEmitter {
	return &progressEmitter{observer: observer, project: project, started: started}
}

func (e *progressEmitter) emit(phase ProgressPhase, state ProgressState, unit string, completed, total int, message string) error {
	if e == nil || e.observer == nil {
		return nil
	}
	event := ProgressEvent{
		Schema: ProgressSchemaV1, RepositoryID: e.project.ID, RepositoryName: e.project.Name,
		Branch: e.project.Branch, Phase: phase, State: state, Unit: unit,
		Completed: completed, Total: total, ElapsedMS: time.Since(e.started).Milliseconds(),
		RebuildReason: e.rebuild, Error: message,
	}
	if err := e.observer(event); err != nil {
		return fmt.Errorf("observe indexing progress %s/%s: %w", phase, state, err)
	}
	return nil
}

func (e *progressEmitter) terminal(ctx context.Context, runErr error) error {
	state := ProgressCompleted
	message := ""
	if runErr != nil {
		state = ProgressError
		message = ProgressErrorMessage(runErr, e.project.IndexPath, e.project.Root)
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			state = ProgressCanceled
		}
	}
	return e.emit(ProgressComplete, state, "", 0, 0, message)
}

// ProgressErrorMessage retains an actionable bounded error while removing
// caller-supplied repository paths from machine and human progress output.
func ProgressErrorMessage(err error, sensitivePaths ...string) string {
	message := strings.NewReplacer("\r", " ", "\n", " ").Replace(err.Error())
	sensitive := make([]string, 0, len(sensitivePaths)*3)
	seen := map[string]bool{}
	for _, path := range sensitivePaths {
		for _, candidate := range progressSensitivePathCandidates(path) {
			if candidate != "" && candidate != "." && !seen[candidate] {
				seen[candidate] = true
				sensitive = append(sensitive, candidate)
			}
		}
	}
	sort.Slice(sensitive, func(i, j int) bool { return len(sensitive[i]) > len(sensitive[j]) })
	for _, path := range sensitive {
		message = strings.ReplaceAll(message, path, "<repository>")
	}
	const limit = 512
	runes := []rune(message)
	if len(runes) > limit {
		message = string(runes[:limit-1]) + "…"
	}
	return message
}

func progressSensitivePathCandidates(path string) []string {
	if path == "" {
		return nil
	}
	result := []string{filepath.Clean(path)}
	if absolute, err := filepath.Abs(path); err == nil {
		result = append(result, absolute)
	}
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		result = append(result, canonical)
	}
	return result
}
