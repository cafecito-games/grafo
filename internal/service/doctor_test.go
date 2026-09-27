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
	if err := agentinstall.RecordOwnedFile(env, "codex", agentinstall.KindInstructions, target, "guidance"); err != nil {
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
