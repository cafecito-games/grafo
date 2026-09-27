package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cafecito-games/grafo/internal/agentinstall"
	"github.com/cafecito-games/grafo/internal/service"
)

// serviceUsage is printed for an unknown or missing service subcommand.
const serviceUsage = "usage: grafo service add|remove|list|install|status|logs|uninstall|run [path] [options]"

// errDoctorProblems reports an unhealthy installation so scripts can branch on
// the exit status. Every problem has already been printed.
var errDoctorProblems = errors.New("doctor found problems; see the report above")

// service dispatches the background-service subcommands.
func (a *App) service(ctx context.Context, args parsedArguments) error {
	environment := agentinstall.NewOSEnvironment()
	subcommand := ""
	rest := args.positionals
	if len(rest) > 0 {
		subcommand, rest = rest[0], rest[1:]
	}
	switch subcommand {
	case "add":
		return a.serviceAdd(environment, args, rest)
	case "remove":
		return a.serviceRemove(environment, rest)
	case "list":
		return a.serviceList(environment, args)
	case "install":
		return a.serviceInstall(ctx, environment, args)
	case "uninstall":
		return a.serviceUninstall(ctx, environment, args)
	case "status":
		return a.serviceStatus(ctx, environment, args)
	case "logs":
		return a.serviceLogs(environment, args)
	case "run":
		return a.serviceRun(ctx, environment, args)
	case "":
		return errors.New(serviceUsage)
	default:
		return fmt.Errorf("unknown service subcommand %q (%s)", subcommand, serviceUsage)
	}
}

func (a *App) serviceAdd(environment agentinstall.Environment, args parsedArguments, rest []string) error {
	root, err := optionalPath(rest)
	if err != nil {
		return err
	}
	settings := service.Settings{Paused: args.flags["paused"]}
	if value := strings.TrimSpace(args.values["interval"]); value != "" {
		interval, parseErr := time.ParseDuration(value)
		if parseErr != nil {
			return fmt.Errorf("--interval must be a duration such as 10s")
		}
		settings.Interval = interval
	}
	registry, changed, err := service.NewStore(environment).Add(root, settings)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, registry)
	}
	canonical, _, _ := service.CanonicalRoot(root)
	if changed {
		a.printf("registered %s\n", canonical)
	} else {
		a.printf("already registered %s\n", canonical)
	}
	a.println("run 'grafo service install' once to keep every registered root indexed in the background")
	return nil
}

func (a *App) serviceRemove(environment agentinstall.Environment, rest []string) error {
	root, err := optionalPath(rest)
	if err != nil {
		return err
	}
	_, changed, err := service.NewStore(environment).Remove(root)
	if err != nil {
		return err
	}
	if changed {
		a.printf("unregistered %s (indexes were left in place)\n", root)
		return nil
	}
	a.printf("%s was not registered\n", root)
	return nil
}

func (a *App) serviceList(environment agentinstall.Environment, args parsedArguments) error {
	store := service.NewStore(environment)
	registry, err := store.Load()
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, registry)
	}
	path, err := store.Path()
	if err != nil {
		return err
	}
	a.printf("registry %s\n", path)
	if len(registry.Roots) == 0 {
		a.println("no repository roots are registered; add one with 'grafo service add <path>'")
		return nil
	}
	for _, entry := range registry.Roots {
		state := "watched"
		if entry.Paused {
			state = "paused"
		}
		a.printf("%-8s %-10s %s\n", state, entry.Every(), entry.Root)
	}
	return nil
}

func (a *App) serviceInstall(ctx context.Context, environment agentinstall.Environment, args parsedArguments) error {
	binary, err := installedExecutable(environment)
	if err != nil {
		return err
	}
	stateDir, err := a.stateDir(environment, args)
	if err != nil {
		return err
	}
	actions, installErr := service.Install(ctx, environment, binary, stateDir, args.flags["dry-run"])
	if err := a.printServiceActions(actions, args.flags["json"]); err != nil {
		return err
	}
	return installErr
}

func (a *App) serviceUninstall(ctx context.Context, environment agentinstall.Environment, args parsedArguments) error {
	actions, uninstallErr := service.Uninstall(ctx, environment, args.flags["dry-run"])
	if err := a.printServiceActions(actions, args.flags["json"]); err != nil {
		return err
	}
	return uninstallErr
}

// serviceStatusOutput is the machine-readable shape of `grafo service status`.
type serviceStatusOutput struct {
	Registry service.Registry `json:"registry"`
	Service  service.State    `json:"service"`
	Roots    service.Snapshot `json:"supervisor"`
	Log      string           `json:"log"`
}

func (a *App) serviceStatus(ctx context.Context, environment agentinstall.Environment, args parsedArguments) error {
	store := service.NewStore(environment)
	registry, err := store.Load()
	if err != nil {
		return err
	}
	stateDir, err := a.stateDir(environment, args)
	if err != nil {
		return err
	}
	state, err := service.Describe(ctx, environment)
	if err != nil {
		return err
	}
	snapshot, err := service.ReadSnapshot(service.StatusPath(stateDir))
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, serviceStatusOutput{
			Registry: registry, Service: state, Roots: snapshot, Log: service.LogPath(stateDir),
		})
	}
	installed := "not installed"
	if state.Installed {
		installed = "installed"
		if state.Running {
			installed += ", running"
		} else if state.Loaded {
			installed += ", loaded"
		}
		if state.Conflict {
			installed += " (not owned by grafo)"
		}
	}
	a.printf("platform  %s %s %s\n", state.Platform, installed, state.Definition)
	if state.Detail != "" {
		a.printf("detail    %s\n", state.Detail)
	}
	if snapshot.UpdatedAt != "" {
		a.printf("last pass %s (pid %d)\n", snapshot.UpdatedAt, snapshot.PID)
	} else {
		a.println("last pass none reported yet")
	}
	for _, entry := range registry.Roots {
		status := "no pass yet"
		for _, reported := range snapshot.Roots {
			if reported.Root != entry.Root {
				continue
			}
			switch {
			case reported.LastError != "":
				status = "error: " + reported.LastError
			case reported.LastSuccessAt != "":
				status = fmt.Sprintf("indexed %s branch=%s updated=%d removed=%d",
					reported.LastSuccessAt, reported.Branch, reported.Updated, reported.Removed)
			}
		}
		if entry.Paused {
			status = "paused"
		}
		a.printf("root      %s  %s\n", entry.Root, status)
	}
	a.printf("log       %s\n", service.LogPath(stateDir))
	return nil
}

func (a *App) serviceLogs(environment agentinstall.Environment, args parsedArguments) error {
	stateDir, err := a.stateDir(environment, args)
	if err != nil {
		return err
	}
	lines, err := intOption(args, "lines", 50)
	if err != nil {
		return err
	}
	records, err := service.TailLog(service.LogPath(stateDir), lines)
	if err != nil {
		return err
	}
	if args.flags["json"] {
		return writeJSON(a.stdout, records)
	}
	if len(records) == 0 {
		a.printf("no service log yet at %s\n", service.LogPath(stateDir))
		return nil
	}
	for _, record := range records {
		a.println(record)
	}
	return nil
}

// serviceRun is the supervisor entry point the generated service definition
// starts. It also runs in the foreground, which is how the service is tested and
// debugged; --once performs a single reconciliation pass and exits.
func (a *App) serviceRun(ctx context.Context, environment agentinstall.Environment, args parsedArguments) error {
	stateDir, err := a.stateDir(environment, args)
	if err != nil {
		return err
	}
	concurrency, err := intOption(args, "concurrency", 0)
	if err != nil {
		return err
	}
	logger, err := service.OpenLogger(service.LogPath(stateDir))
	if err != nil {
		return err
	}
	defer func() { _ = logger.Close() }()
	binary, _ := os.Executable()
	supervisor := service.NewSupervisor(service.Options{
		Env: environment, Store: service.NewStore(environment), StateDir: stateDir,
		Logger: logger, Concurrency: concurrency, Binary: binary,
	})
	if args.flags["once"] {
		snapshot, runErr := supervisor.Once(ctx)
		if args.flags["json"] {
			if err := writeJSON(a.stdout, snapshot); err != nil {
				return err
			}
			return runErr
		}
		for _, status := range snapshot.Roots {
			a.printf("%-40s branch=%s updated=%d removed=%d %s\n",
				status.Root, status.Branch, status.Updated, status.Removed, status.LastError)
		}
		return runErr
	}
	// A supervised process is stopped with a signal, so the pass in flight gets to
	// finish its transaction instead of being interrupted mid-write.
	signalled, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return supervisor.Run(signalled)
}

func (a *App) printServiceActions(actions []service.Action, asJSON bool) error {
	if asJSON {
		return writeJSON(a.stdout, actions)
	}
	for _, action := range actions {
		change := action.Change
		if action.DryRun {
			verb, known := plannedVerbs[change]
			if !known {
				verb = change
			}
			change = "would " + verb
		}
		a.printf("%-14s %-10s %s", change, action.Kind, action.Target)
		if action.Detail != "" {
			a.printf(" · %s", action.Detail)
		}
		a.println()
	}
	return nil
}

// doctor inspects the installation and, only with --repair, performs the
// enumerated safe repairs.
func (a *App) doctor(ctx context.Context, args parsedArguments) error {
	environment := agentinstall.NewOSEnvironment()
	stateDir, err := a.stateDir(environment, args)
	if err != nil {
		return err
	}
	binary, _ := os.Executable()
	diagnosis, diagnoseErr := service.Diagnose(ctx, environment, service.DoctorOptions{
		Binary: binary, StateDir: stateDir, Repair: args.flags["repair"],
	})
	if args.flags["json"] {
		if err := writeJSON(a.stdout, diagnosis); err != nil {
			return err
		}
	} else {
		diagnosis.Fprint(a.stdout)
	}
	if diagnoseErr != nil {
		return diagnoseErr
	}
	if !diagnosis.Healthy {
		return errDoctorProblems
	}
	return nil
}

// stateDir resolves the Grafo state directory, honouring the stable --state-dir
// path that generated service definitions pass.
func (a *App) stateDir(environment agentinstall.Environment, args parsedArguments) (string, error) {
	if value := strings.TrimSpace(args.values["state-dir"]); value != "" {
		return value, nil
	}
	return service.StateDir(environment)
}

// installedExecutable resolves the running binary and refuses one that will not
// survive the command, so a service definition never points at a path the Go
// toolchain deletes. The rule itself lives in internal/service, so installation
// and doctor repair cannot disagree about it.
func installedExecutable(environment agentinstall.Environment) (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate grafo executable: %w", err)
	}
	return service.InstallableBinary(environment, executable)
}
