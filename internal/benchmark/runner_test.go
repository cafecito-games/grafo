package benchmark

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
)

func TestRunRejectsInvalidInputBeforeCreatingOutput(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("not a repository"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, repository := range map[string]string{
		"absent":        "",
		"missing":       filepath.Join(t.TempDir(), "missing"),
		"not_directory": file,
		"not_git":       t.TempDir(),
	} {
		t.Run(name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "output")
			_, err := Run(context.Background(), Options{Repository: repository, Output: output})
			if err == nil {
				t.Fatal("expected invalid repository error")
			}
			if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
				t.Fatalf("output created for invalid input: %v", statErr)
			}
		})
	}
}

func TestRunRejectsOutputInsideSourceBeforeCreatingArtifacts(t *testing.T) {
	repository := fixtureRepository(t)
	alias := filepath.Join(t.TempDir(), "corpus-alias")
	if err := os.Symlink(repository, alias); err != nil {
		t.Fatal(err)
	}
	for name, output := range map[string]string{
		"exact": repository,
		"child": filepath.Join(repository, "benchmark-output"),
		"alias": alias,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Run(context.Background(), Options{Repository: repository, Output: output})
			if err == nil {
				t.Fatal("expected unsafe output error")
			}
			if matches, globErr := filepath.Glob(filepath.Join(repository, "artifacts-*")); globErr != nil || len(matches) != 0 {
				t.Fatalf("unsafe artifacts created: matches=%v err=%v", matches, globErr)
			}
			if _, statErr := os.Stat(filepath.Join(repository, ReportFileName)); !os.IsNotExist(statErr) {
				t.Fatalf("unsafe report created: %v", statErr)
			}
		})
	}
}

func TestRunRejectsDefaultTemporaryOutputInsideSource(t *testing.T) {
	repository := fixtureRepository(t)
	temporaryRoot := filepath.Join(repository, "ignored-temp")
	if err := os.WriteFile(filepath.Join(repository, ".git", "info", "exclude"), []byte("ignored-temp/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(temporaryRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", temporaryRoot)

	_, err := Run(context.Background(), Options{Repository: repository})
	if err == nil {
		t.Fatal("expected unsafe default output error")
	}
	entries, readErr := os.ReadDir(temporaryRoot)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("default output artifacts created inside source: %v", entries)
	}
}

func TestRestoreTrackedFilePreservesExecutableMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.py")
	if err := restoreTrackedFile(path, []byte("print('ok')\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("restored mode = %04o, want 0755", got)
	}
}

func TestRunRestoresExecutableMutationTarget(t *testing.T) {
	repository := t.TempDir()
	runTestGit(t, repository, "init", "-b", "main")
	target := filepath.Join(repository, "tool.py")
	if err := os.WriteFile(target, []byte("#!/usr/bin/env python3\nprint('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repository, "add", "--chmod=+x", "tool.py")
	runTestGit(t, repository, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "executable fixture")
	before := sourceState(t, repository)

	report, err := Run(context.Background(), Options{Repository: repository, Output: filepath.Join(t.TempDir(), "benchmark")})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusPassed {
		t.Fatalf("benchmark status = %q, want passed: %s", report.Status, report.Error)
	}
	if after := sourceState(t, repository); !reflect.DeepEqual(before, after) {
		t.Fatalf("source checkout changed: before=%q after=%q", before, after)
	}
}

func TestLoadBaselineRejectsIncompatibleVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":999,"semantic_index_version":"old","graph_schema_version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBaseline(path); err == nil {
		t.Fatal("expected incompatible baseline error")
	}
}

func TestLoadBaselineRejectsFailedReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	report := Report{SchemaVersion: ReportSchemaVersion, SemanticIndexVersion: indexer.SemanticIndexVersion,
		GraphSchemaVersion: graph.SchemaVersion, Status: StatusFailed}
	content, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBaseline(path); err == nil {
		t.Fatal("expected failed baseline rejection")
	}
}

func TestBuildProvenanceIdentifiesGrafoCheckout(t *testing.T) {
	commit, _ := buildProvenance()
	if commit == "" || commit == "unknown" {
		t.Fatalf("missing Grafo commit: %q", commit)
	}
}

func TestRunExercisesIncrementalScenariosWithoutChangingSource(t *testing.T) {
	repository := fixtureRepository(t)
	before := sourceState(t, repository)
	output := filepath.Join(t.TempDir(), "benchmark")

	report, err := Run(context.Background(), Options{Repository: repository, Output: output})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusPassed || report.SchemaVersion != ReportSchemaVersion {
		t.Fatalf("unexpected report status: %#v", report)
	}
	if report.Corpus.Commit == "" || report.Corpus.Branch != "main" {
		t.Fatalf("missing corpus provenance: %#v", report.Corpus)
	}
	for _, language := range []string{"config", "gdscript", "go", "manifest", "sql", "python", "typescript"} {
		if report.Inputs.ByLanguage[language].Files == 0 {
			t.Errorf("missing %s coverage: %#v", language, report.Inputs.ByLanguage)
		}
	}
	if report.Inputs.Unsupported.Files == 0 {
		t.Fatalf("unsupported tracked files are not visible: %#v", report.Inputs)
	}
	if report.Inputs.Skipped.Files != 2 || !reflect.DeepEqual(report.Inputs.SkippedPaths, []string{"alias.go", "vendor/generated.go"}) ||
		!reflect.DeepEqual(report.Inputs.SkippedSymlinks, []string{"alias.go"}) {
		t.Fatalf("production-ignored tracked inputs are not visible: %#v", report.Inputs)
	}

	scenarios := map[string]ScenarioReport{}
	for _, scenario := range report.Scenarios {
		scenarios[scenario.Name] = scenario
		if scenario.Status != StatusPassed {
			t.Errorf("scenario %s failed: %s", scenario.Name, scenario.Error)
		}
	}
	for _, name := range []string{
		"cold", "unchanged", "interrupted", "resumed", "resume_unchanged",
		"edit", "edit_unchanged", "delete", "restore", "restore_unchanged",
		"branch_switch", "branch_restore", "branch_unchanged",
	} {
		if _, ok := scenarios[name]; !ok {
			t.Errorf("missing scenario %q", name)
		}
	}
	if scenarios["unchanged"].Index.ContentChecked != 0 || scenarios["unchanged"].Index.Updated != 0 {
		t.Fatalf("unchanged refresh read content: %#v", scenarios["unchanged"].Index)
	}
	if !reflect.DeepEqual(scenarios["cold"].Index.Counts, scenarios["resumed"].Index.Counts) {
		t.Fatalf("resume did not converge: cold=%#v resumed=%#v", scenarios["cold"].Index.Counts, scenarios["resumed"].Index.Counts)
	}
	if scenarios["resume_unchanged"].Index.ReconciliationBatches != 0 {
		t.Fatalf("resume left reconciliation work: %#v", scenarios["resume_unchanged"].Index)
	}
	cold := scenarios["cold"]
	if cold.Index.Skipped != 1 || !reflect.DeepEqual(cold.Index.SkippedPaths, []string{"alias.go"}) {
		t.Fatalf("tracked symlink was not surfaced as skipped: %#v", cold.Index)
	}
	if cold.Index.Counts.Facts == 0 {
		t.Fatalf("fact count was not reported: %#v", cold.Index.Counts)
	}
	if cold.Index.Writes.Nodes.Rows == 0 || cold.Index.Writes.Facts.Rows == 0 || cold.Index.Writes.Edges.Rows == 0 ||
		cold.Index.Writes.Nodes.Batches == 0 || cold.Index.Writes.Facts.Batches == 0 || cold.Index.Writes.Edges.Batches == 0 {
		t.Fatalf("bounded write metrics were not reported: %#v", cold.Index.Writes)
	}
	if cold.Index.Phases.DiscoveryNS <= 0 || cold.Index.Phases.ReadHashNS <= 0 || cold.Index.Phases.ParseNS <= 0 ||
		cold.Index.Phases.PersistenceNS <= 0 || cold.Index.Phases.ReconciliationNS <= 0 || cold.Index.Phases.TotalNS <= 0 {
		t.Fatalf("phase durations were not reported: %#v", cold.Index.Phases)
	}
	if scenarios["interrupted"].Index.Phases.TotalNS <= 0 {
		t.Fatalf("interrupted scenario omitted total duration: %#v", scenarios["interrupted"].Index.Phases)
	}
	if !cold.Resources.PeakWALBytes.Supported || cold.Resources.PeakWALBytes.Value == nil ||
		!cold.Resources.FinalWALBytes.Supported || cold.Resources.FinalWALBytes.Value == nil {
		t.Fatalf("WAL metrics are not explicit: %#v", cold.Resources)
	}
	if scenarios["edit"].Index.Updated != 1 || scenarios["delete"].Index.Removed != 1 || scenarios["restore"].Index.Updated != 1 {
		t.Fatalf("tracked-file scenarios were not bounded: edit=%#v delete=%#v restore=%#v",
			scenarios["edit"].Index, scenarios["delete"].Index, scenarios["restore"].Index)
	}
	if after := sourceState(t, repository); !reflect.DeepEqual(before, after) {
		t.Fatalf("source checkout changed: before=%q after=%q", before, after)
	}
	if _, err := os.Stat(filepath.Join(repository, ".grafo")); !os.IsNotExist(err) {
		t.Fatalf("source checkout received an index: %v", err)
	}
	secret, err := os.ReadFile(filepath.Join(repository, "secret.env"))
	if err != nil || string(secret) != "DO_NOT_READ=secret\n" {
		t.Fatalf("ignored source file changed: content=%q err=%v", secret, err)
	}

	encoded, err := os.ReadFile(filepath.Join(output, ReportFileName))
	if err != nil {
		t.Fatal(err)
	}
	var persisted Report
	if err := json.Unmarshal(encoded, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Status != StatusPassed || len(persisted.Scenarios) != len(report.Scenarios) {
		t.Fatalf("persisted report differs: %#v", persisted)
	}
	if persisted.Artifacts.ColdDatabase != "" {
		t.Fatalf("successful run retained duplicate control database: %s", persisted.Artifacts.ColdDatabase)
	}
	if _, err := os.Stat(persisted.Artifacts.ResumeDatabase); err != nil {
		t.Fatalf("converged database was not retained: %v", err)
	}
	if matches, err := filepath.Glob(filepath.Join(output, ".*.tmp")); err != nil || len(matches) != 0 {
		t.Fatalf("atomic report left temporary files: matches=%v err=%v", matches, err)
	}
}

func fixtureRepository(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	runTestGit(t, root, "init", "-b", "main")
	files := map[string]string{
		".gitignore":          "secret.env\n",
		"README.md":           "fixture\n",
		"go.mod":              "module example.com/fixture\n\ngo 1.26\n",
		"main.go":             "package fixture\nfunc Value() int { return 1 }\n",
		"worker.py":           "def run():\n    return True\n",
		"web.ts":              "export function run(): boolean { return true }\n",
		"player.gd":           "class_name Player\nfunc run():\n\tpass\n",
		"schema.sql":          "CREATE TABLE events (id bigint PRIMARY KEY);\n",
		"settings.yaml":       "service:\n  enabled: true\n",
		"package.json":        "{\"name\":\"fixture\",\"dependencies\":{}}\n",
		"vendor/generated.go": "package generated\n",
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "secret.env"), []byte("DO_NOT_READ=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("main.go", filepath.Join(root, "alias.go")); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, root, "add", ".")
	runTestGit(t, root, "-c", "user.name=Grafo Test", "-c", "user.email=grafo@example.invalid", "commit", "-m", "fixture")
	return root
}

func sourceState(t *testing.T, root string) []string {
	t.Helper()
	commands := [][]string{{"rev-parse", "HEAD"}, {"symbolic-ref", "--quiet", "--short", "HEAD"}, {"status", "--porcelain=v1", "--untracked-files=all"}}
	state := make([]string, 0, len(commands))
	for _, arguments := range commands {
		command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
		output, err := command.Output()
		if err != nil {
			t.Fatalf("git %v: %v", arguments, err)
		}
		state = append(state, string(output))
	}
	return state
}

func runTestGit(t *testing.T, root string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", arguments, err, output)
	}
}
