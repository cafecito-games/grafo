package service

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/agentinstall"
	"github.com/cafecito-games/grafo/internal/indexer"
)

// doctorOptions builds options with the agent seams stubbed, so a diagnosis
// never depends on which MCP clients happen to be installed on the host.
func doctorOptions(binary string) DoctorOptions {
	return DoctorOptions{
		Binary: binary,
		Discover: func(_ context.Context, root string) (indexer.Project, error) {
			return fakeProject(root, "main"), nil
		},
		DetectAgents: func(context.Context, agentinstall.Environment) ([]agentinstall.Status, error) {
			return nil, nil
		},
		RefreshAgents: func(context.Context, agentinstall.Environment, string) ([]agentinstall.Action, error) {
			return nil, nil
		},
	}
}

// installedBinary creates a file that stands in for an installed grafo binary.
func installedBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grafo")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// findingWith locates a finding by area and a fragment of its detail, so a test
// asserts about the condition it cares about rather than about ordering.
func findingWith(diagnosis Diagnosis, area, detail string) (Finding, bool) {
	index := slices.IndexFunc(diagnosis.Findings, func(finding Finding) bool {
		return finding.Area == area && strings.Contains(finding.Detail, detail)
	})
	if index == -1 {
		return Finding{}, false
	}
	return diagnosis.Findings[index], true
}

func findingFor(diagnosis Diagnosis, area, target string) (Finding, bool) {
	index := slices.IndexFunc(diagnosis.Findings, func(finding Finding) bool {
		return finding.Area == area && (target == "" || finding.Target == target)
	})
	if index == -1 {
		return Finding{}, false
	}
	return diagnosis.Findings[index], true
}

func TestDoctorReportsHealthyRootWithoutRepairing(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	store := NewStore(env)
	root := t.TempDir()
	if _, _, err := store.Add(root, Settings{}); err != nil {
		t.Fatal(err)
	}
	// An index for the active branch makes the root fully healthy.
	indexPath := fakeProject(root, "main").IndexPath
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("index"), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnosis, err := Diagnose(context.Background(), env, doctorOptions(installedBinary(t)))
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if len(diagnosis.Roots) != 1 || !diagnosis.Roots[0].IndexPresent || diagnosis.Roots[0].Branch != "main" {
		t.Fatalf("root diagnosis = %#v", diagnosis.Roots)
	}
	for _, finding := range diagnosis.Findings {
		if finding.Area == "root" || finding.Area == "index" || finding.Area == "registry" {
			t.Fatalf("healthy root produced %#v", finding)
		}
	}
	if len(diagnosis.Repairs) != 0 {
		t.Fatalf("a read-only diagnosis performed repairs: %#v", diagnosis.Repairs)
	}
	// Repairing a healthy installation is a no-op.
	options := doctorOptions(installedBinary(t))
	options.Repair = true
	repaired, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatalf("Diagnose --repair: %v", err)
	}
	for _, action := range repaired.Repairs {
		if action.Change != ChangeUnchanged {
			t.Fatalf("repair on a healthy installation changed %#v", action)
		}
	}
	registry, err := store.Load()
	if err != nil || len(registry.Roots) != 1 {
		t.Fatalf("registry = %#v (%v)", registry, err)
	}
}

func TestDoctorPrunesMissingRootOnlyWithRepair(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	store := NewStore(env)
	root := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Add(root, Settings{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	binary := installedBinary(t)
	diagnosis, err := Diagnose(context.Background(), env, doctorOptions(binary))
	if err != nil {
		t.Fatal(err)
	}
	finding, ok := findingFor(diagnosis, "root", root)
	if !ok || finding.Repair != RepairPruneRoot {
		t.Fatalf("expected a prunable root finding, got %#v", diagnosis.Findings)
	}
	if registry, err := store.Load(); err != nil || len(registry.Roots) != 1 {
		t.Fatalf("read-only doctor changed the registry: %#v (%v)", registry, err)
	}
	options := doctorOptions(binary)
	options.Repair = true
	repaired, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatal(err)
	}
	if index := slices.IndexFunc(repaired.Repairs, func(action Action) bool {
		return action.Kind == RepairPruneRoot && action.Target == root
	}); index == -1 {
		t.Fatalf("repair did not prune the missing root: %#v", repaired.Repairs)
	}
	registry, err := store.Load()
	if err != nil || len(registry.Roots) != 0 {
		t.Fatalf("registry after prune = %#v (%v)", registry, err)
	}
	// Replaying the repair is a no-op.
	again, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Repairs) != 0 {
		t.Fatalf("replayed repair acted again: %#v", again.Repairs)
	}
}

func TestDoctorNeverPrunesAnOverlappingRoot(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	store := NewStore(env)
	parent := t.TempDir()
	child := filepath.Join(parent, "nested")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{parent, child} {
		if _, _, err := store.Add(root, Settings{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(child); err != nil {
		t.Fatal(err)
	}
	options := doctorOptions(installedBinary(t))
	options.Repair = true
	diagnosis, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatal(err)
	}
	finding, ok := findingFor(diagnosis, "root", child)
	if !ok {
		t.Fatalf("missing nested root was not reported: %#v", diagnosis.Findings)
	}
	if finding.Repair != "" {
		t.Fatalf("an overlapping root must not be prunable: %#v", finding)
	}
	if finding.Manual == "" {
		t.Fatal("an unrepairable condition must report a manual step")
	}
	registry, err := store.Load()
	if err != nil || len(registry.Roots) != 2 {
		t.Fatalf("registry = %#v (%v)", registry, err)
	}
}

func TestDoctorRecreatesOnlyAServiceDefinitionGrafoInstalled(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	stateDir, err := StateDir(env)
	if err != nil {
		t.Fatal(err)
	}
	binary := installedBinary(t)
	if _, err := Install(context.Background(), env, binary, stateDir, false); err != nil {
		t.Fatal(err)
	}
	definition, err := PlatformFor("linux").DefinitionPath(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(definition); err != nil {
		t.Fatal(err)
	}
	diagnosis, err := Diagnose(context.Background(), env, doctorOptions(binary))
	if err != nil {
		t.Fatal(err)
	}
	finding, ok := findingFor(diagnosis, "service", definition)
	if !ok || finding.Repair != RepairServiceDefinition {
		t.Fatalf("expected a recreatable definition finding, got %#v", diagnosis.Findings)
	}
	options := doctorOptions(binary)
	options.Repair = true
	repaired, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if _, err := os.Stat(definition); err != nil {
		t.Fatalf("definition was not recreated: %v", err)
	}
	if index := slices.IndexFunc(repaired.Repairs, func(action Action) bool {
		return action.Kind == "definition" && action.Change == ChangeInstalled
	}); index == -1 {
		t.Fatalf("repairs = %#v", repaired.Repairs)
	}
}

func TestDoctorReportsAConflictingDefinitionAndLeavesItAlone(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	definition, err := PlatformFor("linux").DefinitionPath(env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(definition), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "[Service]\nExecStart=/bin/true\n"
	if err := os.WriteFile(definition, []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	options := doctorOptions(installedBinary(t))
	options.Repair = true
	diagnosis, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatal(err)
	}
	finding, ok := findingFor(diagnosis, "service", definition)
	if !ok || finding.Repair != "" || finding.Level != "error" {
		t.Fatalf("expected an unrepairable conflict, got %#v", diagnosis.Findings)
	}
	contents, err := os.ReadFile(definition)
	if err != nil || string(contents) != foreign {
		t.Fatalf("repair rewrote a definition it does not own: %q (%v)", contents, err)
	}
}

func TestDoctorRefreshesOnlyRecordedAgentArtifacts(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	// A receipt proves Grafo installed a guidance file that is now gone.
	target, err := agentinstall.HomeDirPath(env, ".codex", "AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := agentinstall.RecordOwnedFile(env, "codex", agentinstall.KindInstructions, target, resolvedUserTarget(t, env, target), "guidance"); err != nil {
		t.Fatal(err)
	}
	options := doctorOptions(installedBinary(t))
	options.DetectAgents = func(context.Context, agentinstall.Environment) ([]agentinstall.Status, error) {
		return []agentinstall.Status{{
			Client:    agentinstall.Client{Name: "codex", Display: "Codex", Scope: "user", Method: "cli"},
			Installed: true, Registered: true, Path: "/usr/local/bin/codex",
		}}, nil
	}
	diagnosis, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatal(err)
	}
	finding, ok := findingFor(diagnosis, "agents", target)
	if !ok || finding.Repair != RepairRefreshAgents {
		t.Fatalf("expected a refreshable agent artifact, got %#v", diagnosis.Findings)
	}
	refreshed := false
	options.Repair = true
	options.RefreshAgents = func(context.Context, agentinstall.Environment, string) ([]agentinstall.Action, error) {
		refreshed = true
		return []agentinstall.Action{{
			Client: agentinstall.Client{Name: "codex", Display: "Codex"},
			Kind:   agentinstall.KindInstructions, Target: target, Change: ChangeInstalled,
		}}, nil
	}
	if _, err := Diagnose(context.Background(), env, options); err != nil {
		t.Fatal(err)
	}
	if !refreshed {
		t.Fatal("repair did not refresh the recorded agent artifact")
	}
}

func TestDoctorReportsAMalformedRegistryWithRecoverySteps(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	store := NewStore(env)
	path, err := store.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	options := doctorOptions(installedBinary(t))
	options.Repair = true
	diagnosis, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatal(err)
	}
	finding, ok := findingFor(diagnosis, "registry", path)
	if !ok || finding.Level != "error" || !strings.Contains(finding.Manual, "aside") {
		t.Fatalf("expected recovery instructions, got %#v", diagnosis.Findings)
	}
	if diagnosis.Healthy {
		t.Fatal("a malformed registry must not report healthy")
	}
	// The malformed file is left exactly as it was.
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "{oops" {
		t.Fatalf("registry was rewritten: %q (%v)", contents, err)
	}
}

func TestDiagnosisPrintsHumanReadableOutput(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	diagnosis, err := Diagnose(context.Background(), env, doctorOptions(installedBinary(t)))
	if err != nil {
		t.Fatal(err)
	}
	buffer := &bytes.Buffer{}
	diagnosis.Fprint(buffer)
	for _, want := range []string{"binary", "registry", "service", "supervisor"} {
		if !strings.Contains(buffer.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, buffer)
		}
	}
}

// Finding 2: a service receipt proves one location. Doctor must not read it as
// permission to create and activate a definition somewhere it never wrote one.
func TestDoctorIgnoresAServiceReceiptRecordedForAnotherPath(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	resolved, err := PlatformFor("linux").DefinitionPath(env)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := agentinstall.ConfigHomePath(env, "systemd", "user", "other-"+SystemdUnit)
	if err != nil {
		t.Fatal(err)
	}
	if err := agentinstall.RecordOwnedFile(env, agentinstall.ServiceOwner, "systemd", elsewhere, resolvedUserTarget(t, env, elsewhere), "[Service]\n"); err != nil {
		t.Fatal(err)
	}
	// Give the diagnosis a registered root so the service is worth reporting.
	if _, _, err := NewStore(env).Add(t.TempDir(), Settings{}); err != nil {
		t.Fatal(err)
	}
	options := doctorOptions(installedBinary(t))
	diagnosis, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatal(err)
	}
	finding, ok := findingFor(diagnosis, "service", resolved)
	if !ok {
		t.Fatalf("the missing definition was not reported: %#v", diagnosis.Findings)
	}
	if finding.Repair == RepairServiceDefinition {
		t.Fatalf("a receipt for %s authorized recreating %s: %#v", elsewhere, resolved, finding)
	}
	options.Repair = true
	repaired, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired.Repairs) != 0 {
		t.Fatalf("repair acted on an uncovered path: %#v", repaired.Repairs)
	}
	if _, err := os.Stat(resolved); !os.IsNotExist(err) {
		t.Fatalf("a definition was created at an uncovered path: %v", err)
	}
	env.mutex.Lock()
	defer env.mutex.Unlock()
	for _, command := range env.commands {
		if strings.Contains(command, "enable --now") || strings.Contains(command, "bootstrap") {
			t.Fatalf("repair activated a service it never installed: %#v", env.commands)
		}
	}
}

// Finding 6: an executable path that merely extends the current one is a
// different executable.
func TestDoctorDetectsADefinitionWhoseExecutableExtendsTheCurrentBinary(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	stateDir, err := StateDir(env)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	current := filepath.Join(directory, "grafo")
	extended := filepath.Join(directory, "grafo-next")
	for _, path := range []string{current, extended} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The installed definition starts grafo-next, whose path contains the path of
	// the binary now running.
	if _, err := Install(context.Background(), env, extended, stateDir, false); err != nil {
		t.Fatal(err)
	}
	diagnosis, err := Diagnose(context.Background(), env, doctorOptions(current))
	if err != nil {
		t.Fatal(err)
	}
	definition, err := PlatformFor("linux").DefinitionPath(env)
	if err != nil {
		t.Fatal(err)
	}
	finding, ok := findingWith(diagnosis, "service", "different grafo binary")
	if !ok || finding.Repair != RepairServiceDefinition || finding.Target != definition {
		t.Fatalf("a definition starting %s was reported correct for %s: %#v", extended, current, diagnosis.Findings)
	}
	// Repairing rewrites the definition to the binary that is actually running.
	options := doctorOptions(current)
	options.Repair = true
	if _, err := Diagnose(context.Background(), env, options); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(definition)
	if err != nil {
		t.Fatal(err)
	}
	declared, ok := DefinitionBinary("linux", string(contents))
	if !ok || declared != current {
		t.Fatalf("definition still starts %q, want %q", declared, current)
	}
}

// Finding 4: repairing from a `go run` build would install a service that breaks
// as soon as the command exits.
func TestDoctorRefusesToRepairFromAnEphemeralBinary(t *testing.T) {
	env := newRecordingEnvironment(t, "linux")
	definition, err := PlatformFor("linux").DefinitionPath(env)
	if err != nil {
		t.Fatal(err)
	}
	// A receipt that covers the resolved path makes the missing definition
	// repairable in principle, so only the binary rule can stop it.
	generated, err := PlatformFor("linux").Definition(filepath.Join(t.TempDir(), "grafo"), "/state")
	if err != nil {
		t.Fatal(err)
	}
	if err := agentinstall.RecordOwnedFile(env, agentinstall.ServiceOwner, "systemd", definition, resolvedUserTarget(t, env, definition), generated); err != nil {
		t.Fatal(err)
	}
	ephemeral := filepath.Join(env.TempDir(), "go-build4242", "b001", "exe", "grafo")
	if err := os.MkdirAll(filepath.Dir(ephemeral), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ephemeral, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	options := doctorOptions(ephemeral)
	diagnosis, err := Diagnose(context.Background(), env, options)
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.Binary.Serviceable {
		t.Fatalf("a build-directory binary was reported serviceable: %#v", diagnosis.Binary)
	}
	if finding, ok := findingFor(diagnosis, "binary", ephemeral); !ok || !strings.Contains(finding.Manual, "go install") {
		t.Fatalf("the ephemeral binary was not reported with a remedy: %#v", diagnosis.Findings)
	}
	options.Repair = true
	repaired, repairErr := Diagnose(context.Background(), env, options)
	if repairErr == nil {
		t.Fatal("expected repair to refuse an ephemeral binary")
	}
	if !strings.Contains(repairErr.Error(), "cannot recreate the service definition") {
		t.Fatalf("repair error = %v", repairErr)
	}
	if _, err := os.Stat(definition); !os.IsNotExist(err) {
		t.Fatalf("a definition pointing into a build directory was written: %v", err)
	}
	for _, action := range repaired.Repairs {
		if action.Kind == "definition" || action.Kind == "activate" {
			t.Fatalf("repair acted anyway: %#v", action)
		}
	}
}

// A correct, freshly installed definition must produce no binary finding at all.
// The mismatch check parses the definition, so it has to agree with the generator
// about both the syntax and which platform produced it.
func TestDoctorAcceptsADefinitionThatStartsTheRunningBinary(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			env := newRecordingEnvironment(t, goos)
			stateDir, err := StateDir(env)
			if err != nil {
				t.Fatal(err)
			}
			binary := installedBinary(t)
			if _, err := Install(context.Background(), env, binary, stateDir, false); err != nil {
				t.Fatal(err)
			}
			diagnosis, err := Diagnose(context.Background(), env, doctorOptions(binary))
			if err != nil {
				t.Fatal(err)
			}
			if finding, ok := findingWith(diagnosis, "service", "different grafo binary"); ok {
				t.Fatalf("a correct definition was reported as pointing elsewhere: %#v", finding)
			}
		})
	}
}
