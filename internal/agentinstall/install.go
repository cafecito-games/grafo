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

// Action is what Install/Uninstall did (or, in dry-run, would do) for one
// artifact of one client.
type Action struct {
	Client Client `json:"client"`
	Kind   string `json:"kind"` // KindMCP, KindSkill, KindInstructions, KindHooks
	Scope  string `json:"scope"`
	Target string `json:"target"` // config path, artifact path, or client executable
	Change string `json:"change"` // "installed", "updated", "removed", "unchanged", "skipped"
	DryRun bool   `json:"dry_run,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Options selects clients, selects artifact kinds, and controls mutation.
type Options struct {
	Targets []string // explicit client names; empty or ["all"] means every supported client
	All     bool
	DryRun  bool
	// MCPOnly installs or removes only the MCP registration, never guidance.
	MCPOnly bool
	// Hooks opts in to the advisory, fail-open hooks of clients that document a
	// safe hook API. Uninstall always removes Grafo's hooks regardless.
	Hooks bool
	// Refresh updates only artifacts that are already present, so it never adds
	// a registration, skill, block, or hook that the user has not installed.
	Refresh bool
	// Announce receives the full planned action list before any mutation runs.
	Announce func(planned []Action)

	// executable is the resolved Grafo binary path, set by Install.
	executable string
}

func executableOf(options Options) string { return options.executable }

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

// Install registers executable as the Grafo MCP server for the selected clients
// and installs Grafo's guidance surfaces unless Options.MCPOnly is set. With no
// explicit targets (or Options.All) every supported client is considered and
// genuinely uninstalled ones are reported as skipped.
func Install(ctx context.Context, env Environment, executable string, options Options) ([]Action, error) {
	resolved, err := resolveExecutable(Reader(env), executable)
	if err != nil {
		return nil, err
	}
	options.executable = resolved
	return run(ctx, env, options, true, func(entry adapter, reader Reader, place location) (plan, error) {
		return entry.planInstall(ctx, reader, place, resolved)
	})
}

// Uninstall removes only Grafo's own registration, guidance, and hooks from the
// selected clients. Anything whose ownership cannot be proven is left in place
// and reported as skipped.
func Uninstall(ctx context.Context, env Environment, options Options) ([]Action, error) {
	return run(ctx, env, options, false, func(entry adapter, reader Reader, place location) (plan, error) {
		return entry.planUninstall(ctx, reader, place)
	})
}

type planner func(entry adapter, reader Reader, place location) (plan, error)

// step is one planned artifact mutation: the action to report and the plan that
// realizes it.
type step struct {
	action Action
	plan   plan
	// artifact is nil for the MCP registration.
	artifact guidanceArtifact
}

func run(ctx context.Context, env Environment, options Options, install bool, build planner) ([]Action, error) {
	selected, automatic, err := selectAdapters(options.Targets, options.All)
	if err != nil {
		return nil, err
	}

	// Planning is restricted to the read-only view so a dry run provably cannot
	// reach a mutating operation.
	reader := Reader(env)
	receipts, err := loadReceipts(reader)
	if err != nil {
		return nil, err
	}

	steps, failures := planSteps(ctx, reader, options, install, build, selected, automatic)

	planned := make([]Action, 0, len(steps))
	for _, entry := range steps {
		planned = append(planned, entry.action)
	}
	// Every target and action is reported before anything is mutated.
	if options.Announce != nil {
		options.Announce(planned)
	}
	if options.DryRun {
		return planned, errors.Join(append(failures, noClientError(automatic, planned, failures))...)
	}

	actions := make([]Action, 0, len(steps))
	for _, entry := range steps {
		// Skipped and unchanged artifacts are reported without touching the
		// filesystem, the client, or the receipt ledger.
		if entry.action.Change == changeUnchanged || entry.action.Change == changeSkipped {
			actions = append(actions, entry.action)
			continue
		}
		if applyErr := entry.plan.apply(ctx, env, entry.action.Client.Display, entry.action.Target); applyErr != nil {
			failures = append(failures, applyErr)
			continue
		}
		// Receipts are updated only after the corresponding mutation succeeded.
		switch {
		case entry.plan.dropReceipt:
			receipts.drop(entry.action.Client, entry.action.Kind)
		case entry.plan.change == changeRemoved:
			receipts.drop(entry.action.Client, entry.action.Kind)
		default:
			receipts.record(entry.action.Client, entry.action.Kind, entry.action.Target, entry.plan.digest)
		}
		actions = append(actions, entry.action)
	}
	if saveErr := receipts.save(env); saveErr != nil {
		failures = append(failures, saveErr)
	}
	return actions, errors.Join(append(failures, noClientError(automatic, actions, failures))...)
}

// planSteps builds every intended action without mutating anything.
func planSteps(ctx context.Context, reader Reader, options Options, install bool, build planner,
	selected []adapter, automatic bool) ([]step, []error) {
	var steps []step
	var failures []error
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
			steps = append(steps, step{action: Action{
				Client: identity, Kind: KindMCP, Scope: identity.Scope, Target: place.target,
				Change: changeSkipped, DryRun: options.DryRun, Detail: place.detail,
			}})
			continue
		}

		intended, planErr := build(entry, reader, place)
		if planErr != nil {
			failures = append(failures, planErr)
		} else {
			steps = append(steps, newStep(identity, KindMCP, place.target, intended, options, nil))
		}
		if options.MCPOnly {
			continue
		}
		guidanceSteps, guidanceFailures := planGuidance(reader, options, install, identity)
		steps = append(steps, guidanceSteps...)
		failures = append(failures, guidanceFailures...)
	}
	return steps, failures
}

// planGuidance plans the guidance artifacts of one detected client.
func planGuidance(reader Reader, options Options, install bool, identity Client) ([]step, []error) {
	var steps []step
	var failures []error
	for _, artifact := range guidanceFor(identity.Name) {
		target, pathErr := artifact.path(reader)
		if pathErr != nil {
			failures = append(failures, fmt.Errorf("%s %s: %w", identity.Display, artifact.kind(), pathErr))
			continue
		}
		// Opt-in artifacts are reported as explicitly skipped on install so the
		// user can see the capability exists; uninstall always cleans them up.
		if install && artifact.optIn() && !options.Hooks {
			steps = append(steps, step{action: Action{
				Client: identity, Kind: artifact.kind(), Scope: identity.Scope, Target: target,
				Change: changeSkipped, DryRun: options.DryRun,
				Detail: "advisory hooks were not requested (pass --hooks)",
			}})
			continue
		}
		var intended plan
		var planErr error
		if install {
			intended, planErr = artifact.planInstall(reader, identity.Display, target, executableOf(options))
		} else {
			intended, planErr = artifact.planUninstall(reader, identity.Display, target)
		}
		if planErr != nil {
			failures = append(failures, planErr)
			continue
		}
		steps = append(steps, newStep(identity, artifact.kind(), target, intended, options, artifact))
	}
	return steps, failures
}

// newStep pairs a plan with the action that reports it, applying --refresh.
func newStep(identity Client, kind, target string, intended plan, options Options, artifact guidanceArtifact) step {
	if options.Refresh && intended.change == changeInstalled {
		intended = plan{
			change: changeSkipped,
			detail: "not installed yet; run without --refresh to add it",
		}
	}
	return step{
		action: Action{
			Client: identity, Kind: kind, Scope: identity.Scope, Target: target,
			Change: intended.change, DryRun: options.DryRun, Detail: intended.detail,
		},
		plan:     intended,
		artifact: artifact,
	}
}

// noClientError reports the automatic-selection case where nothing was found.
func noClientError(automatic bool, actions []Action, failures []error) error {
	if !automatic || len(failures) > 0 {
		return nil
	}
	for _, action := range actions {
		if action.Change != changeSkipped {
			return nil
		}
	}
	return fmt.Errorf("no supported MCP clients detected (supported: %s)", strings.Join(clientNames(), ", "))
}
