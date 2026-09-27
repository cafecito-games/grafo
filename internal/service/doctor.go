package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/agentinstall"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/version"
)

// diagnosisFormat is the JSON format marker of `grafo doctor --json`.
const diagnosisFormat = "grafo.doctor/1"

// StaleAfter is how long a supervisor may go without updating its status before
// it is reported stale.
const StaleAfter = 10 * time.Minute

// Repair names. Exactly these repairs exist; anything else is reported with a
// manual step instead.
const (
	RepairPruneRoot         = "prune-missing-root"
	RepairRefreshAgents     = "refresh-agent-registration"
	RepairServiceDefinition = "recreate-service-definition"
	RepairRestartService    = "restart-stale-service"
)

// Finding is one diagnosed condition.
type Finding struct {
	Area   string `json:"area"`
	Level  string `json:"level"` // "error" or "warning"
	Target string `json:"target,omitempty"`
	Detail string `json:"detail"`
	// Repair names the enumerated repair that fixes this finding, or "" when only
	// a manual step can.
	Repair string `json:"repair,omitempty"`
	// Manual is the actionable step for a condition Grafo must not repair itself.
	Manual string `json:"manual,omitempty"`
	// Repaired reports that --repair fixed this finding in this run.
	Repaired bool `json:"repaired,omitempty"`
}

// BinaryCheck reports the Grafo binary a service definition can point at.
type BinaryCheck struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	Installed bool   `json:"installed"`
	Detail    string `json:"detail,omitempty"`
}

// RegistryCheck reports the watched-root registry itself.
type RegistryCheck struct {
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	Roots    int    `json:"roots"`
	Detail   string `json:"detail,omitempty"`
}

// RootCheck reports one registered root.
type RootCheck struct {
	Root         string      `json:"root"`
	Name         string      `json:"name,omitempty"`
	Paused       bool        `json:"paused,omitempty"`
	Exists       bool        `json:"exists"`
	Readable     bool        `json:"readable"`
	GitManaged   bool        `json:"git_managed"`
	Branch       string      `json:"branch,omitempty"`
	IndexPath    string      `json:"index_path,omitempty"`
	IndexPresent bool        `json:"index_present"`
	IndexBytes   int64       `json:"index_bytes,omitempty"`
	SharedWith   []string    `json:"shared_with,omitempty"`
	Status       *RootStatus `json:"status,omitempty"`
	Detail       string      `json:"detail,omitempty"`
}

// SupervisorCheck reports the running supervisor, as far as its status file and
// the init system can tell.
type SupervisorCheck struct {
	StatusPath string `json:"status_path"`
	Present    bool   `json:"status_present"`
	PID        int    `json:"pid,omitempty"`
	Binary     string `json:"binary,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	Stale      bool   `json:"stale,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// AgentCheck reports one supported MCP client's Grafo registration and the
// guidance artifacts Grafo recorded installing for it.
type AgentCheck struct {
	Client     string `json:"client"`
	Display    string `json:"display"`
	Installed  bool   `json:"installed"`
	Registered bool   `json:"registered"`
	Path       string `json:"path,omitempty"`
	Artifacts  []struct {
		Kind    string `json:"kind"`
		Target  string `json:"target"`
		Present bool   `json:"present"`
		Matches bool   `json:"matches"`
	} `json:"artifacts,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Diagnosis is the whole doctor report.
type Diagnosis struct {
	Format     string          `json:"format"`
	Binary     BinaryCheck     `json:"binary"`
	Registry   RegistryCheck   `json:"registry"`
	Roots      []RootCheck     `json:"roots"`
	Service    State           `json:"service"`
	Supervisor SupervisorCheck `json:"supervisor"`
	Agents     []AgentCheck    `json:"agents"`
	Findings   []Finding       `json:"findings"`
	// Repairs lists what --repair actually did, in order.
	Repairs []Action `json:"repairs,omitempty"`
	Healthy bool     `json:"healthy"`
}

// DoctorOptions configures a diagnosis.
type DoctorOptions struct {
	// Binary is the absolute path of the running Grafo binary.
	Binary string
	// StateDir overrides the Grafo state directory.
	StateDir string
	// Repair applies the enumerated repairs. Doctor is read-only without it.
	Repair bool
	// Discover resolves branch identity; defaults to indexer.DiscoverProject.
	Discover Discoverer
	// DetectAgents reports supported MCP clients read-only; defaults to
	// agentinstall.Detect over the whole registry. Doctor never extends that
	// client list.
	DetectAgents func(ctx context.Context, env agentinstall.Environment) ([]agentinstall.Status, error)
	// RefreshAgents refreshes only the Grafo artifacts a client already has;
	// defaults to agentinstall.Install with Options.Refresh.
	RefreshAgents func(ctx context.Context, env agentinstall.Environment, binary string) ([]agentinstall.Action, error)
}

// Diagnose inspects the binary, the registry, every registered root, the
// platform service, the supervisor status and the agent registrations. Without
// DoctorOptions.Repair it mutates nothing at all.
func Diagnose(ctx context.Context, env agentinstall.Environment, options DoctorOptions) (Diagnosis, error) {
	if options.Discover == nil {
		options.Discover = indexer.DiscoverProject
	}
	if options.DetectAgents == nil {
		options.DetectAgents = func(ctx context.Context, env agentinstall.Environment) ([]agentinstall.Status, error) {
			return agentinstall.Detect(ctx, env, nil)
		}
	}
	if options.RefreshAgents == nil {
		options.RefreshAgents = func(ctx context.Context, env agentinstall.Environment, binary string) ([]agentinstall.Action, error) {
			return agentinstall.Install(ctx, env, binary, agentinstall.Options{Refresh: true})
		}
	}
	stateDir := options.StateDir
	if stateDir == "" {
		resolved, err := StateDir(env)
		if err != nil {
			return Diagnosis{}, err
		}
		stateDir = resolved
	}
	diagnosis := Diagnosis{Format: diagnosisFormat, Roots: []RootCheck{}, Agents: []AgentCheck{}, Findings: []Finding{}}
	store := NewStore(env)

	diagnosis.Binary = checkBinary(env, options.Binary, &diagnosis)
	registry := checkRegistry(store, &diagnosis)
	snapshot, snapshotErr := ReadSnapshot(StatusPath(stateDir))
	if snapshotErr != nil {
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "supervisor", Level: "error", Target: StatusPath(stateDir), Detail: snapshotErr.Error(),
			Manual: "move the status file aside; the supervisor rewrites it on its next pass",
		})
	}
	diagnosis.Roots = checkRoots(ctx, env, options, registry, snapshot, &diagnosis)
	diagnosis.Supervisor = checkSupervisor(stateDir, snapshot, snapshotErr, registry)

	state, err := Describe(ctx, env)
	if err != nil {
		return diagnosis, err
	}
	diagnosis.Service = state
	checkService(env, state, registry, diagnosis.Binary, &diagnosis)
	checkStaleSupervisor(state, diagnosis.Supervisor, &diagnosis)
	diagnosis.Agents = checkAgents(ctx, env, options, &diagnosis)

	if options.Repair {
		repairs, err := repair(ctx, env, store, options, stateDir, &diagnosis)
		diagnosis.Repairs = repairs
		if err != nil {
			return diagnosis, err
		}
	}
	diagnosis.Healthy = true
	for _, finding := range diagnosis.Findings {
		if finding.Level == "error" && !finding.Repaired {
			diagnosis.Healthy = false
		}
	}
	return diagnosis, nil
}

func checkBinary(env agentinstall.Environment, binary string, diagnosis *Diagnosis) BinaryCheck {
	check := BinaryCheck{Path: binary, Version: version.Value}
	if strings.TrimSpace(binary) == "" {
		check.Detail = "the running binary path could not be resolved"
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "binary", Level: "warning", Detail: check.Detail,
			Manual: "re-run doctor from an installed grafo binary",
		})
		return check
	}
	if info, err := env.Stat(binary); err == nil && !info.IsDir() {
		check.Installed = true
	}
	temporary := strings.TrimSpace(env.TempDir())
	if temporary != "" && strings.HasPrefix(binary, filepath.Clean(temporary)+string(filepath.Separator)) {
		check.Detail = "the running binary lives in a temporary directory and cannot back a background service"
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "binary", Level: "warning", Target: binary, Detail: check.Detail,
			Manual: "install grafo with 'go install github.com/cafecito-games/grafo/cmd/grafo@latest' and re-run",
		})
	}
	return check
}

func checkRegistry(store *Store, diagnosis *Diagnosis) Registry {
	path, err := store.Path()
	if err != nil {
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "registry", Level: "error", Detail: err.Error(),
			Manual: "set HOME (or XDG_CONFIG_HOME) to a writable directory",
		})
		return Registry{}
	}
	check := RegistryCheck{Path: path}
	registry, err := store.Load()
	if err != nil {
		check.Detail = err.Error()
		diagnosis.Registry = check
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "registry", Level: "error", Target: path, Detail: err.Error(),
			Manual: "move " + path + " aside, then re-add your roots with 'grafo service add <path>'",
		})
		return Registry{}
	}
	check.Readable, check.Roots = true, len(registry.Roots)
	diagnosis.Registry = check
	if len(registry.Roots) == 0 {
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "registry", Level: "warning", Detail: "no repository roots are registered",
			Manual: "register one with 'grafo service add <path>'",
		})
	}
	return registry
}

func checkRoots(ctx context.Context, env agentinstall.Environment, options DoctorOptions,
	registry Registry, snapshot Snapshot, diagnosis *Diagnosis) []RootCheck {
	byRoot := map[string]RootStatus{}
	for _, status := range snapshot.Roots {
		byRoot[status.Root] = status
	}
	checks := make([]RootCheck, 0, len(registry.Roots))
	for _, entry := range registry.Roots {
		check := RootCheck{Root: entry.Root, Name: entry.Name, Paused: entry.Paused,
			SharedWith: SharedRoots(registry, entry.Root)}
		if status, ok := byRoot[entry.Root]; ok {
			copied := status
			check.Status = &copied
		}
		info, statErr := env.Stat(entry.Root)
		switch {
		case statErr != nil && errors.Is(statErr, fs.ErrNotExist):
			check.Detail = "the repository root no longer exists"
			finding := Finding{Area: "root", Level: "error", Target: entry.Root, Detail: check.Detail}
			if len(check.SharedWith) > 0 {
				finding.Manual = "another registered root overlaps this path (" +
					strings.Join(check.SharedWith, ", ") + "); remove the right one with 'grafo service remove <path>'"
			} else {
				finding.Repair = RepairPruneRoot
				finding.Manual = "if the repository moved, run 'grafo service add <new path>'; 'grafo doctor --repair' only unregisters it"
			}
			diagnosis.Findings = append(diagnosis.Findings, finding)
			checks = append(checks, check)
			continue
		case statErr != nil:
			check.Exists = true
			check.Detail = statErr.Error()
			diagnosis.Findings = append(diagnosis.Findings, Finding{
				Area: "root", Level: "error", Target: entry.Root, Detail: statErr.Error(),
				Manual: "fix the permissions on " + entry.Root + "; Grafo keeps its registration and indexes",
			})
			checks = append(checks, check)
			continue
		case !info.IsDir():
			check.Exists = true
			check.Detail = "the registered root is not a directory"
			diagnosis.Findings = append(diagnosis.Findings, Finding{
				Area: "root", Level: "error", Target: entry.Root, Detail: check.Detail,
				Manual: "unregister it with 'grafo service remove " + entry.Root + "'",
			})
			checks = append(checks, check)
			continue
		}
		check.Exists, check.Readable = true, true
		project, discoverErr := options.Discover(ctx, entry.Root)
		if discoverErr != nil || strings.TrimSpace(project.Branch) == "" {
			detail := "the active branch could not be determined"
			if discoverErr != nil {
				detail = discoverErr.Error()
			}
			check.Detail = detail
			diagnosis.Findings = append(diagnosis.Findings, Finding{
				Area: "root", Level: "error", Target: entry.Root, Detail: detail,
				Manual: "run 'git status' in " + entry.Root + "; Grafo never indexes into a fallback branch database",
			})
			checks = append(checks, check)
			continue
		}
		check.GitManaged, check.Branch, check.IndexPath = project.GitManaged, project.Branch, project.IndexPath
		if info, err := env.Stat(project.IndexPath); err == nil {
			check.IndexPresent, check.IndexBytes = true, info.Size()
		} else if !entry.Paused {
			diagnosis.Findings = append(diagnosis.Findings, Finding{
				Area: "index", Level: "warning", Target: project.IndexPath,
				Detail: "the active branch has no index yet",
				Manual: "run 'grafo index " + entry.Root + "' once, or wait for the service to index it",
			})
		}
		if check.Status != nil && check.Status.LastError != "" {
			diagnosis.Findings = append(diagnosis.Findings, Finding{
				Area: "root", Level: "warning", Target: entry.Root,
				Detail: "the last supervisor pass failed: " + check.Status.LastError,
				Manual: "see 'grafo service logs'",
			})
		}
		checks = append(checks, check)
	}
	return checks
}

func checkSupervisor(stateDir string, snapshot Snapshot, snapshotErr error, registry Registry) SupervisorCheck {
	check := SupervisorCheck{StatusPath: StatusPath(stateDir)}
	if snapshotErr != nil {
		check.Detail = snapshotErr.Error()
		return check
	}
	if snapshot.UpdatedAt == "" {
		check.Detail = "the supervisor has not reported a pass yet"
		return check
	}
	check.Present, check.PID, check.Binary = true, snapshot.PID, snapshot.Binary
	check.StartedAt, check.UpdatedAt = snapshot.StartedAt, snapshot.UpdatedAt
	updated, parseErr := time.Parse(time.RFC3339, snapshot.UpdatedAt)
	if parseErr != nil {
		check.Detail = "the status timestamp is unreadable"
		return check
	}
	if now().Sub(updated) > StaleAfter && len(registry.Roots) > 0 {
		check.Stale = true
		check.Detail = "the supervisor has not reported a pass since " + snapshot.UpdatedAt
	}
	return check
}

func checkService(env agentinstall.Environment, state State, registry Registry, binary BinaryCheck, diagnosis *Diagnosis) {
	if !state.Supported {
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "service", Level: "warning", Detail: state.Detail,
			Manual: "run 'grafo watch' in a foreground terminal on this platform",
		})
		return
	}
	switch {
	case !state.Installed:
		// A receipt for a definition that is no longer on disk is the one case
		// where Grafo can recreate it: it proves Grafo installed it before.
		_, found, _ := agentinstall.OwnedFile(env, agentinstall.ServiceOwner, state.Platform)
		finding := Finding{
			Area: "service", Level: "warning", Target: state.Definition,
			Detail: "no service definition is installed, so registered roots are only indexed in the foreground",
			Manual: "run 'grafo service install'",
		}
		if found {
			finding.Level = "error"
			finding.Detail = "the service definition Grafo installed is missing"
			finding.Repair = RepairServiceDefinition
		}
		if found || len(registry.Roots) > 0 {
			diagnosis.Findings = append(diagnosis.Findings, finding)
		}
	case state.Installed && state.Conflict:
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "service", Level: "error", Target: state.Definition,
			Detail: "the service definition differs from Grafo's generated content and Grafo cannot prove it wrote it",
			Manual: "move " + state.Definition + " aside, then run 'grafo service install'",
		})
	case state.Installed && !state.Loaded:
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "service", Level: "warning", Target: state.Definition,
			Detail: "the service definition is installed but the init system has not loaded it",
			Repair: RepairServiceDefinition,
			Manual: "run 'grafo service install' to reload it",
		})
	}
	if state.Installed && state.Owned && binary.Path != "" && !state.pointsAt(env, binary.Path) {
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "service", Level: "error", Target: state.Definition,
			Detail: "the service definition points at a different grafo binary than the one running",
			Repair: RepairServiceDefinition,
			Manual: "run 'grafo service install' from the binary you want the service to use",
		})
	}
}

// checkStaleSupervisor reports a service that the init system has loaded but
// whose supervisor stopped reporting passes. Restarting it is safe because the
// committed indexes are intact and the next pass reconciles from them.
func checkStaleSupervisor(state State, supervisor SupervisorCheck, diagnosis *Diagnosis) {
	if !state.Supported || !state.Installed || !state.Loaded {
		return
	}
	switch {
	case supervisor.Present && supervisor.Stale:
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "supervisor", Level: "error", Target: Label,
			Detail: supervisor.Detail, Repair: RepairRestartService,
			Manual: "restart the service through your init system",
		})
	case !state.Running:
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "supervisor", Level: "error", Target: Label,
			Detail: "the service is loaded but not running", Repair: RepairRestartService,
			Manual: "restart the service through your init system",
		})
	}
}

func checkAgents(ctx context.Context, env agentinstall.Environment, options DoctorOptions, diagnosis *Diagnosis) []AgentCheck {
	statuses, detectErr := options.DetectAgents(ctx, env)
	if detectErr != nil {
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "agents", Level: "warning", Detail: detectErr.Error(),
			Manual: "run 'grafo install --list' for the full client report",
		})
	}
	receipts, receiptErr := agentinstall.Receipts(env)
	if receiptErr != nil {
		diagnosis.Findings = append(diagnosis.Findings, Finding{
			Area: "agents", Level: "error", Detail: receiptErr.Error(),
			Manual: "move the receipt file aside, then re-run 'grafo install'",
		})
	}
	checks := make([]AgentCheck, 0, len(statuses))
	for _, status := range statuses {
		check := AgentCheck{
			Client: status.Client.Name, Display: status.Client.Display,
			Installed: status.Installed, Registered: status.Registered,
			Path: status.Path, Detail: status.Detail,
		}
		for _, receipt := range receipts {
			if receipt.Client != status.Client.Name || receipt.Kind == agentinstall.KindMCP {
				continue
			}
			artifact := struct {
				Kind    string `json:"kind"`
				Target  string `json:"target"`
				Present bool   `json:"present"`
				Matches bool   `json:"matches"`
			}{Kind: receipt.Kind, Target: receipt.Target}
			contents, err := env.ReadFile(receipt.Target)
			if err == nil {
				artifact.Present = true
				artifact.Matches = agentinstall.ProvesFile(receipt, receipt.Target, string(contents))
			}
			check.Artifacts = append(check.Artifacts, artifact)
			if !artifact.Present {
				diagnosis.Findings = append(diagnosis.Findings, Finding{
					Area: "agents", Level: "warning", Target: receipt.Target,
					Detail: status.Client.Display + " " + receipt.Kind + " Grafo installed is missing",
					Repair: RepairRefreshAgents,
					Manual: "run 'grafo install --client " + status.Client.Name + "'",
				})
			}
		}
		if status.Installed && !status.Registered {
			finding := Finding{
				Area: "agents", Level: "warning", Target: status.Path,
				Detail: status.Client.Display + " is installed but Grafo is not registered as an MCP server",
				Manual: "run 'grafo install --client " + status.Client.Name + "'",
			}
			// Only a registration Grafo recorded once may be refreshed for the user;
			// a client that never had it is left to an explicit install.
			if slices.ContainsFunc(receipts, func(receipt agentinstall.Receipt) bool {
				return receipt.Client == status.Client.Name && receipt.Kind == agentinstall.KindMCP
			}) {
				finding.Repair = RepairRefreshAgents
			}
			diagnosis.Findings = append(diagnosis.Findings, finding)
		}
		checks = append(checks, check)
	}
	sort.Slice(checks, func(left, right int) bool { return checks[left].Client < checks[right].Client })
	return checks
}

// repair applies only the enumerated repairs for findings that named one. Every
// repair is idempotent, and none of them touches user-authored content.
func repair(ctx context.Context, env agentinstall.Environment, store *Store, options DoctorOptions,
	stateDir string, diagnosis *Diagnosis) ([]Action, error) {
	var actions []Action
	var failures []error
	applied := map[string]bool{}
	for index := range diagnosis.Findings {
		finding := &diagnosis.Findings[index]
		switch finding.Repair {
		case RepairPruneRoot:
			// Pruning is allowed only for a root that is definitively gone and
			// overlaps no other registration; indexes are never deleted.
			if _, err := env.Stat(finding.Target); err == nil || !errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if _, changed, err := store.Remove(finding.Target); err != nil {
				failures = append(failures, err)
				actions = append(actions, Action{Kind: RepairPruneRoot, Target: finding.Target, Change: ChangeSkipped, Detail: err.Error()})
				continue
			} else if changed {
				finding.Repaired = true
				actions = append(actions, Action{Kind: RepairPruneRoot, Target: finding.Target, Change: ChangeRemoved,
					Detail: "unregistered a missing root; its indexes were left untouched"})
			}
		case RepairRefreshAgents:
			if applied[RepairRefreshAgents] {
				finding.Repaired = true
				continue
			}
			applied[RepairRefreshAgents] = true
			if diagnosis.Binary.Path == "" || !diagnosis.Binary.Installed {
				failures = append(failures, errors.New("cannot refresh agent registrations without an installed grafo binary"))
				continue
			}
			// Refresh updates only artifacts that already exist, so no client gains
			// a registration or a guidance file it never had.
			refreshed, err := options.RefreshAgents(ctx, env, diagnosis.Binary.Path)
			for _, action := range refreshed {
				if action.Change == ChangeUnchanged || action.Change == ChangeSkipped {
					continue
				}
				actions = append(actions, Action{Kind: RepairRefreshAgents, Target: action.Target,
					Change: action.Change, Detail: action.Client.Display + " " + action.Kind})
			}
			if err != nil {
				failures = append(failures, err)
				continue
			}
			finding.Repaired = true
		case RepairServiceDefinition:
			if applied[RepairServiceDefinition] {
				finding.Repaired = true
				continue
			}
			applied[RepairServiceDefinition] = true
			if diagnosis.Binary.Path == "" || !diagnosis.Binary.Installed {
				failures = append(failures, errors.New("cannot recreate the service definition without an installed grafo binary"))
				continue
			}
			installed, err := Install(ctx, env, diagnosis.Binary.Path, stateDir, false)
			actions = append(actions, installed...)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			finding.Repaired = true
		case RepairRestartService:
			if applied[RepairRestartService] {
				finding.Repaired = true
				continue
			}
			applied[RepairRestartService] = true
			platform := PlatformFor(env.GOOS())
			if err := platform.Restart(ctx, env); err != nil {
				failures = append(failures, err)
				actions = append(actions, Action{Platform: platform.Name(), Kind: RepairRestartService,
					Target: Label, Change: ChangeSkipped, Detail: err.Error()})
				continue
			}
			finding.Repaired = true
			actions = append(actions, Action{Platform: platform.Name(), Kind: RepairRestartService,
				Target: Label, Change: ChangeUpdated, Detail: "restarted a stale supervisor"})
		}
	}
	return actions, errors.Join(failures...)
}

// pointsAt reports whether an installed definition names this binary. It reads
// the definition Grafo owns, so a foreign file is never parsed for meaning.
func (s State) pointsAt(reader agentinstall.Reader, binary string) bool {
	if s.Definition == "" || !s.Owned {
		return true
	}
	contents, err := reader.ReadFile(s.Definition)
	if err != nil {
		return true
	}
	return strings.Contains(string(contents), binary)
}

// Fprint writes a human-readable diagnosis.
func (d Diagnosis) Fprint(out interface{ Write([]byte) (int, error) }) {
	write := func(format string, arguments ...any) { fmt.Fprintf(out, format, arguments...) }
	write("binary      %s (grafo %s)\n", orDash(d.Binary.Path), d.Binary.Version)
	registryState := "unreadable"
	if d.Registry.Readable {
		registryState = fmt.Sprintf("%d root(s)", d.Registry.Roots)
	}
	write("registry    %s  %s\n", orDash(d.Registry.Path), registryState)
	serviceState := "not installed"
	switch {
	case !d.Service.Supported:
		serviceState = "unsupported platform"
	case d.Service.Installed && d.Service.Running:
		serviceState = "installed, running"
	case d.Service.Installed && d.Service.Loaded:
		serviceState = "installed, loaded"
	case d.Service.Installed:
		serviceState = "installed, not loaded"
	}
	write("service     %-10s %s  %s\n", d.Service.Platform, serviceState, orDash(d.Service.Definition))
	supervisor := "no status reported"
	if d.Supervisor.Present {
		supervisor = "last pass " + d.Supervisor.UpdatedAt
		if d.Supervisor.Stale {
			supervisor += " (stale)"
		}
	}
	write("supervisor  %s\n", supervisor)
	for _, root := range d.Roots {
		state := "ok"
		switch {
		case !root.Exists:
			state = "missing"
		case !root.Readable:
			state = "unreadable"
		case root.Paused:
			state = "paused"
		case !root.IndexPresent:
			state = "no index"
		}
		write("root        %-9s %s  branch=%s\n", state, root.Root, orDash(root.Branch))
	}
	for _, agent := range d.Agents {
		state := "not installed"
		if agent.Installed && agent.Registered {
			state = "registered"
		} else if agent.Installed {
			state = "grafo not registered"
		}
		write("agent       %-14s %s\n", agent.Client, state)
	}
	for _, finding := range d.Findings {
		marker := finding.Level
		if finding.Repaired {
			marker = "repaired"
		}
		write("%-11s %s: %s\n", marker, finding.Area, finding.Detail)
		if finding.Manual != "" && !finding.Repaired {
			write("            -> %s\n", finding.Manual)
		}
	}
	for _, action := range d.Repairs {
		write("repair      %-28s %s %s\n", action.Kind, action.Change, orDash(action.Target))
	}
	if d.Healthy && len(d.Findings) == 0 {
		write("healthy     no problems found\n")
	}
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
