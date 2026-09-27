// Package agentinstall registers Grafo's MCP server with supported MCP clients.
//
// The package is built from three parts: a declarative client registry
// (registry.go), adapters that either drive a client's own CLI or edit a
// documented user-level configuration file (adapters.go), and the injected
// Environment seam (env.go). Detection and dry-run planning only ever receive
// the read-only half of that seam, so neither can mutate anything.
package agentinstall

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Status is one row of detection output. Detect never mutates anything.
type Status struct {
	Client     Client `json:"client"`
	Installed  bool   `json:"installed"`      // the client itself was found
	Path       string `json:"path,omitempty"` // client executable or config file
	Registered bool   `json:"registered"`     // grafo is already registered
	Detail     string `json:"detail,omitempty"`
}

// Action is what Install/Uninstall did (or, in dry-run, would do) for one client.
type Action struct {
	Client Client `json:"client"`
	Scope  string `json:"scope"`
	Target string `json:"target"` // config path or client executable
	Change string `json:"change"` // "installed", "updated", "removed", "unchanged", "skipped"
	DryRun bool   `json:"dry_run,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Options selects clients and controls mutation.
type Options struct {
	Targets []string // explicit client names; empty or ["all"] means every supported client
	All     bool
	DryRun  bool
}

// Detect reports what is installed and whether Grafo is already registered. It
// uses only the read-only half of the environment and writes nothing.
func Detect(ctx context.Context, env Environment, targets []string) ([]Status, error) {
	selected, _, err := selectAdapters(targets, false)
	if err != nil {
		return nil, err
	}
	return detect(ctx, Reader(env), selected)
}

func detect(ctx context.Context, reader Reader, selected []adapter) ([]Status, error) {
	statuses := make([]Status, 0, len(selected))
	var failures []error
	for _, entry := range selected {
		identity := entry.client()
		place, err := entry.locate(ctx, reader)
		if err != nil {
			failures = append(failures, err)
			statuses = append(statuses, Status{Client: identity, Detail: err.Error()})
			continue
		}
		statuses = append(statuses, Status{
			Client:     identity,
			Installed:  place.found,
			Path:       place.target,
			Registered: place.registered,
			Detail:     place.detail,
		})
	}
	return statuses, errors.Join(failures...)
}

// Install registers executable as the Grafo MCP server for the selected
// clients. With no explicit targets (or Options.All) every supported client is
// considered and genuinely uninstalled ones are reported as skipped.
func Install(ctx context.Context, env Environment, executable string, options Options) ([]Action, error) {
	resolved, err := resolveExecutable(Reader(env), executable)
	if err != nil {
		return nil, err
	}
	return run(ctx, env, options, func(entry adapter, reader Reader, place location) (plan, error) {
		return entry.planInstall(ctx, reader, place, resolved)
	})
}

// Uninstall removes only Grafo's own registration from the selected clients.
func Uninstall(ctx context.Context, env Environment, options Options) ([]Action, error) {
	return run(ctx, env, options, func(entry adapter, reader Reader, place location) (plan, error) {
		return entry.planUninstall(ctx, reader, place)
	})
}

type planner func(entry adapter, reader Reader, place location) (plan, error)

func run(ctx context.Context, env Environment, options Options, build planner) ([]Action, error) {
	selected, automatic, err := selectAdapters(options.Targets, options.All)
	if err != nil {
		return nil, err
	}

	// Planning is restricted to the read-only view so a dry run provably cannot
	// reach a mutating operation.
	reader := Reader(env)

	actions := make([]Action, 0, len(selected))
	var failures []error
	changed := false
	for _, entry := range selected {
		identity := entry.client()
		place, locateErr := entry.locate(ctx, reader)
		if locateErr != nil {
			failures = append(failures, locateErr)
			continue
		}
		if !place.found {
			if !automatic {
				failures = append(failures, fmt.Errorf("%s is not installed", identity.Display))
				continue
			}
			actions = append(actions, Action{
				Client: identity, Scope: identity.Scope, Target: place.target,
				Change: changeSkipped, DryRun: options.DryRun, Detail: place.detail,
			})
			continue
		}

		intended, planErr := build(entry, reader, place)
		if planErr != nil {
			failures = append(failures, planErr)
			continue
		}
		action := Action{
			Client: identity, Scope: identity.Scope, Target: place.target,
			Change: intended.change, DryRun: options.DryRun, Detail: intended.detail,
		}
		if options.DryRun || intended.change == changeUnchanged {
			actions = append(actions, action)
			if intended.change != changeUnchanged {
				changed = true
			}
			continue
		}
		if applyErr := intended.apply(ctx, env, identity.Display, place.target); applyErr != nil {
			failures = append(failures, applyErr)
			continue
		}
		actions = append(actions, action)
		changed = true
	}

	if automatic && !changed && len(failures) == 0 {
		return actions, fmt.Errorf("no supported MCP clients detected (supported: %s)", strings.Join(clientNames(), ", "))
	}
	return actions, errors.Join(failures...)
}
