package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/cafecito-games/grafo/internal/indexer"
)

type progressMode string

const (
	progressAuto  progressMode = "auto"
	progressHuman progressMode = "human"
	progressJSON  progressMode = "json"
	progressOff   progressMode = "off"
)

func parseProgressMode(raw string) (progressMode, error) {
	if raw == "" {
		return progressAuto, nil
	}
	mode := progressMode(raw)
	switch mode {
	case progressAuto, progressHuman, progressJSON, progressOff:
		return mode, nil
	default:
		return "", fmt.Errorf("--progress must be one of auto, human, json, or off")
	}
}

// progressRenderer serializes timer and indexing callbacks so stderr always
// contains whole human lines or whole NDJSON objects.
type progressRenderer struct {
	mu       sync.Mutex
	wg       sync.WaitGroup
	writer   io.Writer
	mode     progressMode
	delay    time.Duration
	timer    *time.Timer
	visible  bool
	closed   bool
	last     indexer.ProgressEvent
	err      error
	terminal bool
	active   bool
}

func newProgressRenderer(writer io.Writer, mode progressMode, terminal bool, delay time.Duration) *progressRenderer {
	renderer := &progressRenderer{writer: writer, mode: mode, delay: delay, active: mode == progressHuman || mode == progressJSON || (mode == progressAuto && terminal)}
	if mode == progressAuto && terminal {
		if delay <= 0 {
			renderer.visible = true
			return renderer
		}
		renderer.wg.Add(1)
		renderer.timer = time.AfterFunc(delay, func() {
			defer renderer.wg.Done()
			renderer.mu.Lock()
			defer renderer.mu.Unlock()
			if renderer.closed || renderer.err != nil {
				return
			}
			renderer.visible = true
			if renderer.last.Schema != "" {
				renderer.err = renderer.renderLocked(renderer.last)
			}
		})
	}
	return renderer
}

func (r *progressRenderer) Observer() indexer.ProgressObserver {
	if !r.active {
		return nil
	}
	return r.Observe
}

func (r *progressRenderer) Observe(event indexer.ProgressEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.last = event
	if event.State == indexer.ProgressError || event.State == indexer.ProgressCanceled {
		r.terminal = true
	}
	switch r.mode {
	case progressOff:
		return nil
	case progressAuto:
		if !r.visible {
			return nil
		}
	}
	r.err = r.renderLocked(event)
	return r.err
}

func (r *progressRenderer) renderLocked(event indexer.ProgressEvent) error {
	if r.mode == progressJSON {
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(r.writer, "%s\n", encoded)
		return err
	}
	if event.Phase == indexer.ProgressComplete && event.State == indexer.ProgressCompleted {
		if event.RebuildReason == "" {
			return nil
		}
		_, err := fmt.Fprintf(r.writer, "refreshed %s: rebuild %s · %dms\n", event.RepositoryName, event.RebuildReason, event.ElapsedMS)
		return err
	}
	if event.State == indexer.ProgressError || event.State == indexer.ProgressCanceled {
		_, err := fmt.Fprintf(r.writer, "%s %s: %s · %dms\n", event.RepositoryName, event.State, event.Error, event.ElapsedMS)
		return err
	}
	if event.State == indexer.ProgressProgress && event.Total > 0 {
		_, err := fmt.Fprintf(r.writer, "%s %s: %d/%d %s · %dms\n", event.RepositoryName, event.Phase, event.Completed, event.Total, event.Unit, event.ElapsedMS)
		return err
	}
	_, err := fmt.Fprintf(r.writer, "%s %s: %s · %dms\n", event.RepositoryName, event.Phase, event.State, event.ElapsedMS)
	return err
}

func (r *progressRenderer) Close() error {
	r.mu.Lock()
	r.closed = true
	if r.timer != nil && r.timer.Stop() {
		r.wg.Done()
	}
	r.mu.Unlock()
	r.wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *progressRenderer) terminalRendered() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminal && r.mode == progressJSON && r.err == nil
}

func (r *progressRenderer) hasTerminal() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.terminal
}

type progressRenderedError struct{ error }
