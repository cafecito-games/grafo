package agentinstall

import (
	"context"
	"encoding/json"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/cafecito-games/grafo/internal/agentguide"
)

const (
	claudeSkill    = linuxHome + "/.claude/skills/grafo/SKILL.md"
	claudeSettings = linuxHome + "/.claude/settings.json"
	codexSkill     = linuxHome + "/.agents/skills/grafo/SKILL.md"
	codexAgents    = linuxHome + "/.codex/AGENTS.md"
	geminiConfig   = linuxHome + "/.gemini/settings.json"
	geminiGuide    = linuxHome + "/.gemini/GEMINI.md"
)

// guidanceEnvironment returns an environment where Claude Code and Codex are the
// only detected clients, so guidance covers both personal skill locations.
func guidanceEnvironment() *fakeEnvironment {
	environment := cliEnvironment(map[string]string{"claude": "/bin/claude", "codex": "/bin/codex"})
	environment.outputs["/bin/claude mcp list"] = ""
	environment.outputs["/bin/codex mcp list"] = ""
	return environment
}

func instructionEnvironment() *fakeEnvironment {
	environment := guidanceEnvironment()
	environment.files[geminiConfig] = "{}\n"
	return environment
}

func findAction(t *testing.T, actions []Action, client, kind string) Action {
	t.Helper()
	matches := actionsOfKind(actions, client, kind)
	if len(matches) != 1 {
		t.Fatalf("actions for %s/%s = %#v, want exactly one", client, kind, matches)
	}
	return matches[0]
}

func TestInstallWritesPersonalSkillsAndMigratesManagedBlock(t *testing.T) {
	environment := guidanceEnvironment()
	original := "# My rules\n\nAlways run the tests.\n"
	environment.files[codexAgents] = original + "\n" + agentguide.Block()

	actions, err := Install(context.Background(), environment, grafoPath, Options{})
	if err != nil {
		t.Fatal(err)
	}

	skill := findAction(t, actions, "claude", KindSkill)
	if skill.Target != claudeSkill || skill.Change != changeInstalled {
		t.Fatalf("skill action = %#v", skill)
	}
	if got := environment.files[claudeSkill]; got != agentguide.Skill() {
		t.Fatalf("skill file = %q", got)
	}
	if !strings.Contains(environment.files[claudeSkill], "get_blast_radius") {
		t.Error("installed skill does not mention impact analysis")
	}

	codex := findAction(t, actions, "codex", KindSkill)
	if codex.Target != codexSkill || codex.Change != changeInstalled {
		t.Fatalf("codex skill action = %#v", codex)
	}
	if got := environment.files[codexSkill]; got != agentguide.Skill() {
		t.Fatalf("codex skill file = %q", got)
	}
	legacy := findAction(t, actions, "codex", KindInstructions)
	if legacy.Target != codexAgents || legacy.Change != changeRemoved {
		t.Fatalf("legacy instructions action = %#v", legacy)
	}
	if got := environment.files[codexAgents]; got != original {
		t.Fatalf("legacy instructions = %q, want %q", got, original)
	}
	if !slicesContainsPath(environment.mkdirs, linuxHome+"/.claude/skills/grafo") {
		t.Fatalf("mkdirs = %v", environment.mkdirs)
	}
	if !slicesContainsPath(environment.mkdirs, linuxHome+"/.agents/skills/grafo") {
		t.Fatalf("mkdirs = %v", environment.mkdirs)
	}
}

func TestCodexMigrationDropsReceiptAfterManagedBlockWasRemoved(t *testing.T) {
	environment := guidanceEnvironment()
	environment.files[codexAgents] = "# My rules\n"
	ledger, err := json.Marshal(receiptFile{Format: receiptFormat, Receipts: []Receipt{{
		Client: "codex", Kind: KindInstructions, Target: codexAgents,
		ResolvedTarget: codexAgents, Digest: agentguide.Digest("stale managed contents"),
		Grafo: "0.1.0", Updated: "2026-01-01T00:00:00Z",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	environment.files[receiptLedger] = string(ledger)

	actions, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := findAction(t, actions, "codex", KindInstructions); got.Change != changeUnchanged {
		t.Fatalf("legacy instructions action = %#v", got)
	}
	receipts, err := Receipts(environment)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts {
		if receipt.Client == "codex" && receipt.Kind == KindInstructions {
			t.Fatalf("stale legacy receipt survived migration: %#v", receipt)
		}
	}
}

func slicesContainsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func TestInstallIsIdempotentAcrossGuidance(t *testing.T) {
	environment := guidanceEnvironment()
	if _, err := Install(context.Background(), environment, grafoPath, Options{}); err != nil {
		t.Fatal(err)
	}
	environment.writes = nil
	actions, err := Install(context.Background(), environment, grafoPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Kind == KindMCP || action.Kind == KindHooks {
			continue
		}
		if action.Change != changeUnchanged {
			t.Errorf("replay changed %s/%s: %q", action.Client.Name, action.Kind, action.Change)
		}
	}
	for _, write := range environment.writes {
		if write.path == claudeSkill || write.path == codexSkill || write.path == codexAgents {
			t.Errorf("replay rewrote %s", write.path)
		}
	}
}

func TestInstallAnnouncesEveryTargetBeforeMutating(t *testing.T) {
	environment := guidanceEnvironment()
	var announced []Action
	var writesAtAnnounce int
	options := Options{Announce: func(planned []Action) {
		announced = planned
		writesAtAnnounce = len(environment.writes) + len(environment.invocations)
	}}

	actions, err := Install(context.Background(), environment, grafoPath, options)
	if err != nil {
		t.Fatal(err)
	}
	if writesAtAnnounce != 0 {
		t.Fatalf("%d mutations happened before the plan was announced", writesAtAnnounce)
	}
	if len(announced) != len(actions) {
		t.Fatalf("announced %d actions, applied %d", len(announced), len(actions))
	}
	for index := range announced {
		if announced[index].Target == "" && announced[index].Change != changeSkipped {
			t.Errorf("announced action %#v has no target", announced[index])
		}
		if !reflect.DeepEqual(announced[index], actions[index]) {
			t.Errorf("announced %#v, applied %#v", announced[index], actions[index])
		}
	}
}

func TestDryRunReportsGuidanceWithoutWriting(t *testing.T) {
	environment := guidanceEnvironment()
	environment.strict = true

	actions, err := Install(context.Background(), environment, grafoPath, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	environment.assertNoMutations(t)
	if _, exists := environment.files[claudeSkill]; exists {
		t.Fatal("dry run created the skill file")
	}
	if _, exists := environment.files[codexSkill]; exists {
		t.Fatal("dry run created the Codex skill file")
	}
	if _, exists := environment.files[receiptLedger]; exists {
		t.Fatal("dry run wrote a receipt")
	}
	skill := findAction(t, actions, "claude", KindSkill)
	if !skill.DryRun || skill.Change != changeInstalled {
		t.Fatalf("skill action = %#v", skill)
	}
}

func TestMCPOnlySkipsGuidance(t *testing.T) {
	environment := guidanceEnvironment()
	actions, err := Install(context.Background(), environment, grafoPath, Options{MCPOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Kind != KindMCP {
			t.Fatalf("--mcp-only planned %#v", action)
		}
	}
	if _, exists := environment.files[claudeSkill]; exists {
		t.Fatal("--mcp-only installed a skill file")
	}
	if _, exists := environment.files[codexSkill]; exists {
		t.Fatal("--mcp-only installed the Codex skill file")
	}
	if _, exists := environment.files[codexAgents]; exists {
		t.Fatal("--mcp-only created an instruction file")
	}
}

func TestRefreshUpdatesOnlyExistingArtifacts(t *testing.T) {
	environment := guidanceEnvironment()
	// A stale Claude skill exists; the Codex skill does not.
	environment.files[claudeSkill] = "---\nname: grafo\n---\n\n<!-- grafo-guidance version 0 -->\nold\n"

	actions, err := Install(context.Background(), environment, grafoPath, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	skill := findAction(t, actions, "claude", KindSkill)
	if skill.Change != changeUpdated {
		t.Fatalf("stale skill was not refreshed: %#v", skill)
	}
	if environment.files[claudeSkill] != agentguide.Skill() {
		t.Fatal("refresh did not replace the stale skill exactly")
	}
	codex := findAction(t, actions, "codex", KindSkill)
	if codex.Change != changeSkipped {
		t.Fatalf("refresh installed a missing artifact: %#v", codex)
	}
	if _, exists := environment.files[codexSkill]; exists {
		t.Fatal("refresh created the Codex skill file")
	}
}

func TestInstallRefusesForeignSkillFile(t *testing.T) {
	environment := guidanceEnvironment()
	environment.files[claudeSkill] = "---\nname: grafo\n---\n\nhand-written guidance\n"
	before := environment.files[claudeSkill]

	_, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"claude"}})
	if err == nil || !strings.Contains(err.Error(), "no Grafo ownership marker") {
		t.Fatalf("error = %v", err)
	}
	if environment.files[claudeSkill] != before {
		t.Fatal("the foreign skill file was modified")
	}
}

func TestInstallRefusesConflictingMarkers(t *testing.T) {
	cases := map[string]string{
		"duplicated": "a\n" + agentguide.Block() + "\n" + agentguide.Block(),
		"incomplete": "a\n" + agentguide.BeginMarker + "\nhalf a block\n",
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			environment := instructionEnvironment()
			environment.files[geminiGuide] = contents

			_, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"gemini"}})
			if err == nil || !strings.Contains(err.Error(), "grafo-guidance markers are") {
				t.Fatalf("error = %v", err)
			}
			if environment.files[geminiGuide] != contents {
				t.Fatal("the conflicting file was rewritten")
			}
		})
	}
}

func TestInstallRefusesUnsafeGuidanceTargets(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		environment := instructionEnvironment()
		environment.symlinks[geminiGuide] = "/etc/passwd"

		_, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"gemini"}})
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("error = %v", err)
		}
		for _, write := range environment.writes {
			if write.path == geminiGuide {
				t.Fatalf("wrote through the symlink: %v", write)
			}
		}
	})
	t.Run("world writable", func(t *testing.T) {
		environment := instructionEnvironment()
		environment.files[geminiGuide] = "# rules\n"
		environment.modes[geminiGuide] = 0o666

		_, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"gemini"}})
		if err == nil || !strings.Contains(err.Error(), "world-writable") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCodexSymlinkedInstructionsDoNotBlockPersonalSkill(t *testing.T) {
	environment := guidanceEnvironment()
	environment.symlinks[codexAgents] = "../.claude/CLAUDE.md"
	environment.files[linuxHome+"/.claude/CLAUDE.md"] = "# Shared instructions\n"

	actions, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := findAction(t, actions, "codex", KindSkill); got.Change != changeInstalled || got.Target != codexSkill {
		t.Fatalf("codex skill action = %#v", got)
	}
	if got := findAction(t, actions, "codex", KindInstructions); got.Change != changeSkipped || !strings.Contains(got.Detail, "symlink") {
		t.Fatalf("legacy instructions action = %#v", got)
	}
	if environment.symlinks[codexAgents] != "../.claude/CLAUDE.md" {
		t.Fatal("the shared instruction symlink changed")
	}
	if environment.files[linuxHome+"/.claude/CLAUDE.md"] != "# Shared instructions\n" {
		t.Fatal("the shared instruction target changed")
	}
	if environment.files[codexSkill] != agentguide.Skill() {
		t.Fatal("Codex did not receive the Grafo personal skill")
	}
}

// A symlinked ancestor must not be able to redirect an approved target out of
// the user configuration tree; the lexical prefix check alone cannot see it.
func TestInstallRefusesSymlinkedParentDirectory(t *testing.T) {
	environment := instructionEnvironment()
	environment.symlinks[linuxHome+"/.gemini"] = "/srv/repo/instructions"
	environment.dirs["/srv/repo/instructions"] = true

	_, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"gemini"}})
	if err == nil || !strings.Contains(err.Error(), "outside the user configuration roots") {
		t.Fatalf("error = %v", err)
	}
	for _, write := range environment.writes {
		if strings.Contains(write.path, "AGENTS.md") {
			t.Fatalf("wrote through a symlinked parent: %v", write)
		}
	}
}

// A symlinked ancestor that still resolves inside the user's own tree is a
// normal dotfiles layout and must keep working.
func TestInstallFollowsSymlinkedParentInsideUserTree(t *testing.T) {
	environment := instructionEnvironment()
	environment.symlinks[linuxHome+"/.gemini"] = linuxHome + "/dotfiles/gemini"
	environment.dirs[linuxHome+"/dotfiles/gemini"] = true

	actions, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"gemini"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := findAction(t, actions, "gemini", KindInstructions); got.Change != changeInstalled {
		t.Fatalf("instructions action = %#v", got)
	}
}

func TestGuidanceTargetsStayInsideUserConfigRoots(t *testing.T) {
	environment := newFakeEnvironment("linux", linuxHome)
	if err := checkUserConfigRoot(environment, "/etc/grafo/AGENTS.md"); err == nil {
		t.Fatal("expected a refusal for a path outside the user roots")
	}
	if err := checkUserConfigRoot(environment, linuxHome+"/../root/.codex/AGENTS.md"); err == nil {
		t.Fatal("expected a refusal for a path containing ..")
	}
	if err := checkUserConfigRoot(environment, codexAgents); err != nil {
		t.Fatalf("home-relative target refused: %v", err)
	}
}

func TestInstallPreservesGuidanceFileMode(t *testing.T) {
	environment := instructionEnvironment()
	environment.files[geminiGuide] = "# rules\n"
	environment.modes[geminiGuide] = 0o600

	if _, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"gemini"}}); err != nil {
		t.Fatal(err)
	}
	for _, write := range environment.writes {
		if write.path == geminiGuide && write.perm != fs.FileMode(0o600) {
			t.Fatalf("wrote %s with mode %04o, want 0600", write.path, write.perm)
		}
	}
}

func TestUninstallRemovesGuidanceAndKeepsUserContent(t *testing.T) {
	environment := guidanceEnvironment()
	original := "# My rules\n\nAlways run the tests.\n"
	environment.files[codexAgents] = original
	if _, err := Install(context.Background(), environment, grafoPath, Options{}); err != nil {
		t.Fatal(err)
	}

	actions, err := Uninstall(context.Background(), environment, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := findAction(t, actions, "codex", KindInstructions); got.Change != changeUnchanged {
		t.Fatalf("retired instructions action = %#v", got)
	}
	if environment.files[codexAgents] != original {
		t.Fatalf("instruction file = %q, want the original %q", environment.files[codexAgents], original)
	}
	if got := findAction(t, actions, "claude", KindSkill); got.Change != changeRemoved {
		t.Fatalf("skill action = %#v", got)
	}
	if _, exists := environment.files[claudeSkill]; exists {
		t.Fatal("the skill file survived uninstall")
	}
	if got := findAction(t, actions, "codex", KindSkill); got.Change != changeRemoved {
		t.Fatalf("codex skill action = %#v", got)
	}
	if _, exists := environment.files[codexSkill]; exists {
		t.Fatal("the Codex skill file survived uninstall")
	}

	// Replay: a second uninstall changes nothing.
	environment.writes = nil
	actions, err = Uninstall(context.Background(), environment, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range actions {
		if action.Kind != KindMCP && action.Change != changeUnchanged {
			t.Errorf("second uninstall reported %s/%s = %q", action.Client.Name, action.Kind, action.Change)
		}
	}
	for _, write := range environment.writes {
		if write.path == codexAgents {
			t.Error("second uninstall rewrote the instruction file")
		}
	}
}

// A user-modified skill file still carries Grafo's marker, but its digest no
// longer matches the receipt, so Grafo cannot prove it wrote the current bytes.
func TestUninstallRefusesModifiedSkillFile(t *testing.T) {
	environment := guidanceEnvironment()
	if _, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	modified := environment.files[claudeSkill] + "\nMy own extra house rule.\n"
	environment.files[claudeSkill] = modified

	actions, err := Uninstall(context.Background(), environment, Options{Targets: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	skill := findAction(t, actions, "claude", KindSkill)
	if skill.Change != changeSkipped || !strings.Contains(skill.Detail, "manually") {
		t.Fatalf("skill action = %#v", skill)
	}
	if environment.files[claudeSkill] != modified {
		t.Fatal("a modified skill file was deleted or rewritten")
	}
}

// Without a receipt there is no proof Grafo wrote the file, even if the marker
// is present, so the file stays.
func TestUninstallRefusesUnreceiptedSkillFile(t *testing.T) {
	environment := guidanceEnvironment()
	environment.files[claudeSkill] = agentguide.Skill()

	actions, err := Uninstall(context.Background(), environment, Options{Targets: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	skill := findAction(t, actions, "claude", KindSkill)
	if skill.Change != changeSkipped || !strings.Contains(skill.Detail, "manually") {
		t.Fatalf("skill action = %#v", skill)
	}
	if environment.files[claudeSkill] != agentguide.Skill() {
		t.Fatal("an unreceipted skill file was deleted")
	}
}

func TestUninstallLeavesUnprovableArtifacts(t *testing.T) {
	environment := guidanceEnvironment()
	environment.files[claudeSkill] = "hand-written skill\n"

	actions, err := Uninstall(context.Background(), environment, Options{Targets: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	skill := findAction(t, actions, "claude", KindSkill)
	if skill.Change != changeSkipped || !strings.Contains(skill.Detail, "manually") {
		t.Fatalf("skill action = %#v", skill)
	}
	if environment.files[claudeSkill] != "hand-written skill\n" {
		t.Fatal("an unowned skill file was modified")
	}
}

func TestUninstallRemovesOnlyTheGrafoBlockFromASharedFile(t *testing.T) {
	environment := guidanceEnvironment()
	environment.files[codexAgents] = "before\n\n" + agentguide.Block() + "\nafter\n"

	if _, err := Uninstall(context.Background(), environment, Options{Targets: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	got := environment.files[codexAgents]
	if strings.Contains(got, agentguide.BeginMarker) {
		t.Fatalf("block survived: %q", got)
	}
	if got != "before\nafter\n" {
		t.Fatalf("remaining file = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Advisory hooks
// ---------------------------------------------------------------------------

func TestHooksAreOptInAndAdvisory(t *testing.T) {
	environment := guidanceEnvironment()
	actions, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	hooks := findAction(t, actions, "claude", KindHooks)
	if hooks.Change != changeSkipped || !strings.Contains(hooks.Detail, "--hooks") {
		t.Fatalf("hooks action = %#v", hooks)
	}
	if _, exists := environment.files[claudeSettings]; exists {
		t.Fatal("hooks were installed without --hooks")
	}

	actions, err = Install(context.Background(), environment, grafoPath,
		Options{Targets: []string{"claude"}, Hooks: true})
	if err != nil {
		t.Fatal(err)
	}
	hooks = findAction(t, actions, "claude", KindHooks)
	if hooks.Change != changeInstalled || !strings.Contains(hooks.Detail, "fail open") {
		t.Fatalf("hooks action = %#v", hooks)
	}
	settings := environment.files[claudeSettings]
	for _, phase := range hookPhases {
		if !strings.Contains(settings, HookCommand("linux", grafoPath, phase.phase)) {
			t.Errorf("settings are missing the %s hook: %s", phase.phase, settings)
		}
	}
	if !strings.Contains(settings, hookEvent) {
		t.Errorf("settings do not use the documented %s event: %s", hookEvent, settings)
	}
}

func TestHooksPreserveForeignEntriesAndRefreshOwnEntries(t *testing.T) {
	environment := guidanceEnvironment()
	environment.files[claudeSettings] = `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/audit"
          }
        ]
      }
    ]
  }
}
`
	options := Options{Targets: []string{"claude"}, Hooks: true}
	if _, err := Install(context.Background(), environment, grafoPath, options); err != nil {
		t.Fatal(err)
	}
	entries := hookEntries(t, environment.files[claudeSettings])
	if len(entries) != 1+len(hookPhases) {
		t.Fatalf("PreToolUse entries = %d, want %d", len(entries), 1+len(hookPhases))
	}
	if entries[0].Matcher != "Bash" || entries[0].Hooks[0].Command != "/usr/local/bin/audit" {
		t.Fatalf("foreign hook entry changed: %#v", entries[0])
	}
	if !strings.Contains(environment.files[claudeSettings], `"model": "opus"`) {
		t.Error("unrelated settings were dropped")
	}

	// Replaying the install leaves the file alone.
	environment.writes = nil
	actions, err := Install(context.Background(), environment, grafoPath, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := findAction(t, actions, "claude", KindHooks); got.Change != changeUnchanged {
		t.Fatalf("replayed hooks action = %#v", got)
	}

	// Moving the binary refreshes Grafo's own entries in place, proven by the
	// receipt, rather than appending a second copy.
	moved := "/opt/elsewhere/grafo"
	actions, err = Install(context.Background(), environment, moved, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := findAction(t, actions, "claude", KindHooks); got.Change != changeUpdated {
		t.Fatalf("moved-binary hooks action = %#v", got)
	}
	entries = hookEntries(t, environment.files[claudeSettings])
	if len(entries) != 1+len(hookPhases) {
		t.Fatalf("refresh duplicated entries: %#v", entries)
	}
	for _, entry := range entries[1:] {
		if !strings.HasPrefix(entry.Hooks[0].Command, moved+" ") {
			t.Errorf("entry was not refreshed to the new path: %#v", entry)
		}
	}

	// Uninstall removes Grafo's entries and keeps the user's.
	if _, err = Uninstall(context.Background(), environment, Options{Targets: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	entries = hookEntries(t, environment.files[claudeSettings])
	if len(entries) != 1 || entries[0].Matcher != "Bash" {
		t.Fatalf("PreToolUse entries after uninstall = %#v", entries)
	}
}

func TestHooksRefuseMalformedSettings(t *testing.T) {
	environment := guidanceEnvironment()
	environment.files[claudeSettings] = "{\"hooks\": "
	_, err := Install(context.Background(), environment, grafoPath,
		Options{Targets: []string{"claude"}, Hooks: true})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v", err)
	}
	if environment.files[claudeSettings] != "{\"hooks\": " {
		t.Fatal("the malformed settings file was rewritten")
	}
}

func hookEntries(t *testing.T, settings string) []hookEntry {
	t.Helper()
	var document struct {
		Hooks map[string][]hookEntry `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(settings), &document); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	return document.Hooks[hookEvent]
}

func TestHookEntryOwnershipIsExact(t *testing.T) {
	owned := []hookPair{{matcher: "Grep|Glob", command: "/opt/bin/grafo guidance --hook pre-search"}}
	cases := map[string]bool{
		`{"matcher":"Grep|Glob","hooks":[{"type":"command","command":"/opt/bin/grafo guidance --hook pre-search"}]}`:                                                     true,
		`{"hooks":[{"command":"/opt/bin/grafo guidance --hook pre-search","type":"command"}],"matcher":"Grep|Glob"}`:                                                     true,
		`{"matcher":"Grep|Glob","hooks":[{"type":"command","command":"/opt/bin/grafo guidance --hook pre-search && evil"}]}`:                                             false,
		`{"matcher":"Bash","hooks":[{"type":"command","command":"/opt/bin/grafo guidance --hook pre-search"}]}`:                                                          false,
		`{"matcher":"Grep|Glob","hooks":[{"type":"command","command":"/opt/bin/grafo guidance --hook pre-search"}],"timeout":5}`:                                         false,
		`{"matcher":"Grep|Glob","hooks":[{"type":"command","command":"/opt/bin/grafo guidance --hook pre-search","timeout":5}]}`:                                         false,
		`{"matcher":"Grep|Glob","hooks":[{"type":"command","command":"/opt/bin/grafo guidance --hook pre-search"},{"type":"command","command":"/usr/local/bin/audit"}]}`: false,
		`{"matcher":"Grep|Glob","hooks":[{"type":"prompt","command":"/opt/bin/grafo guidance --hook pre-search"}]}`:                                                      false,
		`not json`: false,
	}
	for entry, want := range cases {
		if got := hookEntryIsExactly(json.RawMessage(entry), owned); got != want {
			t.Errorf("hookEntryIsExactly(%s) = %v, want %v", entry, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Receipts
// ---------------------------------------------------------------------------

func TestReceiptsRecordOwnershipAfterMutation(t *testing.T) {
	environment := guidanceEnvironment()
	if _, err := Install(context.Background(), environment, grafoPath, Options{}); err != nil {
		t.Fatal(err)
	}
	receipts, err := Receipts(environment)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, receipt := range receipts {
		if receipt.Client != "claude" || receipt.Kind != KindSkill {
			continue
		}
		found = true
		if receipt.Target != claudeSkill {
			t.Errorf("receipt target = %q", receipt.Target)
		}
		if receipt.ResolvedTarget != claudeSkill {
			t.Errorf("receipt resolved target = %q", receipt.ResolvedTarget)
		}
		if receipt.Digest != agentguide.Digest(agentguide.Skill()) {
			t.Errorf("receipt digest = %q", receipt.Digest)
		}
		if receipt.Guidance != agentguide.Version || receipt.Marker != agentguide.BeginMarker {
			t.Errorf("receipt is missing its version marker: %#v", receipt)
		}
		if receipt.Grafo == "" || receipt.Updated == "" {
			t.Errorf("receipt is missing provenance: %#v", receipt)
		}
	}
	if !found {
		t.Fatalf("no skill receipt recorded: %#v", receipts)
	}

	if _, err = Uninstall(context.Background(), environment, Options{}); err != nil {
		t.Fatal(err)
	}
	receipts, err = Receipts(environment)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts {
		if receipt.Client == "claude" && receipt.Kind == KindSkill {
			t.Fatalf("uninstall kept a receipt: %#v", receipt)
		}
	}
}

func TestMalformedReceiptLedgerIsReported(t *testing.T) {
	environment := guidanceEnvironment()
	environment.files[receiptLedger] = "{not json"
	_, err := Install(context.Background(), environment, grafoPath, Options{})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v", err)
	}
	if environment.files[receiptLedger] != "{not json" {
		t.Fatal("the malformed receipt ledger was rewritten")
	}
	if len(environment.writes) != 0 {
		t.Fatalf("writes = %v", environment.writes)
	}
}

func TestUnsupportedReceiptFormatIsReported(t *testing.T) {
	environment := guidanceEnvironment()
	environment.files[receiptLedger] = `{"format":"grafo.install.receipts/99","receipts":[]}`
	_, err := Install(context.Background(), environment, grafoPath, Options{})
	if err == nil || !strings.Contains(err.Error(), "unsupported format") {
		t.Fatalf("error = %v", err)
	}
}

func TestLegacyReceiptRequiresAnUnsymlinkedTarget(t *testing.T) {
	environment := guidanceEnvironment()
	own := ownership{reader: environment, found: true, receipt: Receipt{Target: claudeSettings}}
	if !own.provesPath(claudeSettings) {
		t.Fatal("ordinary unsymlinked legacy receipt stopped proving its target")
	}
	environment.symlinks[linuxHome+"/.claude"] = linuxHome + "/dotfiles/claude"
	environment.dirs[linuxHome+"/dotfiles/claude"] = true
	if own.provesPath(claudeSettings) {
		t.Fatal("legacy receipt followed a parent whose historical destination is unknowable")
	}
}

func TestResolvedUserConfigPathRejectsSymlinkCycles(t *testing.T) {
	environment := newFakeEnvironment("linux", linuxHome)
	environment.symlinks[linuxHome+"/.cursor"] = ".cursor"
	_, err := resolveUserConfigPath(environment, cursorFile)
	if err == nil || !strings.Contains(err.Error(), "too many levels") {
		t.Fatalf("error = %v", err)
	}
}

func TestGuidanceRegistryCoversOnlySupportedClients(t *testing.T) {
	for _, entry := range guidanceRegistry {
		if len(entry.artifacts) == 0 {
			t.Errorf("client %q has an empty guidance list", entry.client)
		}
		found := false
		for _, client := range Clients() {
			if client.Name == entry.client {
				found = true
			}
		}
		if !found {
			t.Errorf("guidance registry names unsupported client %q", entry.client)
		}
	}
}

func TestSkippedClientsLeaveNoReceipt(t *testing.T) {
	environment := guidanceEnvironment()
	if _, err := Install(context.Background(), environment, grafoPath, Options{}); err != nil {
		t.Fatal(err)
	}
	receipts, err := Receipts(environment)
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts {
		if receipt.Target == "" {
			t.Errorf("receipt without a target: %#v", receipt)
		}
		switch {
		case receipt.Client == "claude" && receipt.Kind != KindHooks:
		case receipt.Client == "codex":
		default:
			t.Errorf("receipt for an artifact that was never mutated: %#v", receipt)
		}
	}
}

// Ownership of a hook entry must be exact. A user-authored entry that merely
// starts like Grafo's command, or reuses Grafo's matcher, is not Grafo's.
func TestHooksRefuseToClaimLookalikeEntries(t *testing.T) {
	environment := guidanceEnvironment()
	foreign := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Grep|Glob",
        "hooks": [
          {
            "type": "command",
            "command": "/opt/bin/grafo guidance --hook pre-search && /usr/local/bin/exfiltrate"
          }
        ]
      },
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "/opt/bin/grafo guidance --hook pre-edit"
          }
        ]
      }
    ]
  }
}
`
	environment.files[claudeSettings] = foreign
	options := Options{Targets: []string{"claude"}, Hooks: true}
	if _, err := Install(context.Background(), environment, grafoPath, options); err != nil {
		t.Fatal(err)
	}
	entries := hookEntries(t, environment.files[claudeSettings])
	if len(entries) != 2+len(hookPhases) {
		t.Fatalf("PreToolUse entries = %d, want %d: %s", len(entries), 2+len(hookPhases), environment.files[claudeSettings])
	}
	if !strings.Contains(entries[0].Hooks[0].Command, "exfiltrate") {
		t.Errorf("a lookalike entry was claimed and rewritten: %#v", entries[0])
	}
	if entries[1].Matcher != "Bash" {
		t.Errorf("a user entry reusing Grafo's command shape was claimed: %#v", entries[1])
	}

	if _, err := Uninstall(context.Background(), environment, Options{Targets: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	entries = hookEntries(t, environment.files[claudeSettings])
	if len(entries) != 2 {
		t.Fatalf("uninstall left %d entries, want the 2 user entries: %s", len(entries), environment.files[claudeSettings])
	}
	if !strings.Contains(entries[0].Hooks[0].Command, "exfiltrate") || entries[1].Matcher != "Bash" {
		t.Fatalf("uninstall removed user-authored hooks: %#v", entries)
	}
}

// Uninstall must be driven by what Grafo installed, not by whether the client
// executable is still on PATH, or removing a client orphans its guidance.
func TestUninstallRemovesGuidanceWhenClientExecutableIsGone(t *testing.T) {
	environment := guidanceEnvironment()
	if _, err := Install(context.Background(), environment, grafoPath,
		Options{Targets: []string{"claude", "codex"}, Hooks: true}); err != nil {
		t.Fatal(err)
	}
	// The user uninstalls Claude Code and Codex themselves.
	delete(environment.lookups, "claude")
	delete(environment.lookups, "codex")

	actions, err := Uninstall(context.Background(), environment, Options{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{KindSkill, KindHooks} {
		if got := findAction(t, actions, "claude", kind); got.Change != changeRemoved {
			t.Errorf("claude %s action = %#v", kind, got)
		}
	}
	if got := findAction(t, actions, "codex", KindSkill); got.Change != changeRemoved {
		t.Errorf("codex skill action = %#v", got)
	}
	if got := findAction(t, actions, "codex", KindInstructions); got.Change != changeUnchanged {
		t.Errorf("codex retired instructions action = %#v", got)
	}
	if _, exists := environment.files[claudeSkill]; exists {
		t.Error("the orphaned skill file survived")
	}
	if _, exists := environment.files[codexSkill]; exists {
		t.Error("the orphaned Codex skill survived")
	}
	receipts, err := Receipts(environment)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 0 {
		t.Errorf("orphaned receipts survived: %#v", receipts)
	}
}

// A Grafo path containing a space must produce a hook command a shell can run.
func TestHookCommandQuotesPathsWithSpaces(t *testing.T) {
	cases := []struct {
		goos, executable, want string
	}{
		{"linux", "/opt/bin/grafo", "/opt/bin/grafo guidance --hook pre-edit"},
		{"linux", "/opt/my tools/grafo", "'/opt/my tools/grafo' guidance --hook pre-edit"},
		{"linux", "/opt/o'brien/grafo", `'/opt/o'\''brien/grafo' guidance --hook pre-edit`},
		{"windows", `C:\Program Files\grafo.exe`, `"C:\Program Files\grafo.exe" guidance --hook pre-edit`},
	}
	for _, test := range cases {
		if got := hookCommandFor(test.goos, test.executable, "pre-edit"); got != test.want {
			t.Errorf("hookCommandFor(%q, %q) = %q, want %q", test.goos, test.executable, got, test.want)
		}
	}
}

func TestHooksInstallQuotedExecutablePath(t *testing.T) {
	environment := guidanceEnvironment()
	spaced := "/opt/my tools/grafo"
	options := Options{Targets: []string{"claude"}, Hooks: true}
	if _, err := Install(context.Background(), environment, spaced, options); err != nil {
		t.Fatal(err)
	}
	entries := hookEntries(t, environment.files[claudeSettings])
	if len(entries) != len(hookPhases) {
		t.Fatalf("PreToolUse entries = %#v", entries)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Hooks[0].Command, "'"+spaced+"'") {
			t.Errorf("hook command is not shell-safe: %q", entry.Hooks[0].Command)
		}
	}
	// The quoted form is still recognized as Grafo's own on replay.
	actions, err := Install(context.Background(), environment, spaced, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := findAction(t, actions, "claude", KindHooks); got.Change != changeUnchanged {
		t.Fatalf("replay with a quoted path = %#v", got)
	}
}

// A hook receipt authorizes mutations only at the exact settings file it
// records. A stale receipt from an old configuration location must never let
// install or uninstall touch an identical user-authored entry somewhere else.
func TestHookReceiptDoesNotAuthorizeADifferentSettingsFile(t *testing.T) {
	staleTarget := linuxHome + "/.config/claude/settings.json"
	staleCommands := []string{
		"/old/grafo guidance --hook pre-search",
		"/old/grafo guidance --hook pre-edit",
	}
	// The user hand-wrote an entry that happens to match the stale recording.
	userAuthored := `{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Grep|Glob",
        "hooks": [
          {
            "type": "command",
            "command": "/old/grafo guidance --hook pre-search"
          }
        ]
      }
    ]
  }
}
`
	seed := func(t *testing.T) *fakeEnvironment {
		t.Helper()
		environment := guidanceEnvironment()
		environment.files[claudeSettings] = userAuthored
		ledger, err := json.Marshal(receiptFile{Format: receiptFormat, Receipts: []Receipt{{
			Client: "claude", Kind: KindHooks, Target: staleTarget,
			Commands: staleCommands, Grafo: "0.0.1", Updated: "2020-01-01T00:00:00Z",
		}}})
		if err != nil {
			t.Fatal(err)
		}
		environment.files[receiptLedger] = string(ledger)
		return environment
	}

	t.Run("uninstall", func(t *testing.T) {
		environment := seed(t)
		actions, err := Uninstall(context.Background(), environment, Options{Targets: []string{"claude"}})
		if err != nil {
			t.Fatal(err)
		}
		hooks := findAction(t, actions, "claude", KindHooks)
		if hooks.Change != changeSkipped || !strings.Contains(hooks.Detail, "cannot prove") {
			t.Fatalf("hooks action = %#v", hooks)
		}
		if environment.files[claudeSettings] != userAuthored {
			t.Fatalf("a user-authored hook was removed: %s", environment.files[claudeSettings])
		}
	})

	t.Run("install", func(t *testing.T) {
		environment := seed(t)
		actions, err := Install(context.Background(), environment, grafoPath,
			Options{Targets: []string{"claude"}, Hooks: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := findAction(t, actions, "claude", KindHooks); got.Change != changeInstalled {
			t.Fatalf("hooks action = %#v", got)
		}
		entries := hookEntries(t, environment.files[claudeSettings])
		if len(entries) != 1+len(hookPhases) {
			t.Fatalf("PreToolUse entries = %#v", entries)
		}
		if entries[0].Hooks[0].Command != staleCommands[0] {
			t.Fatalf("a user-authored hook was replaced: %#v", entries[0])
		}
	})
}

func TestHookReceiptDoesNotFollowRetargetedParentInsideUserTree(t *testing.T) {
	environment := guidanceEnvironment()
	options := Options{Targets: []string{"claude"}, Hooks: true}
	if _, err := Install(context.Background(), environment, grafoPath, options); err != nil {
		t.Fatal(err)
	}
	userAuthored := environment.files[claudeSettings]
	environment.symlinks[linuxHome+"/.claude"] = linuxHome + "/dotfiles/claude"
	environment.dirs[linuxHome+"/dotfiles/claude"] = true
	environment.writes = nil
	environment.removes = nil

	actions, err := Uninstall(context.Background(), environment, Options{Targets: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	hooks := findAction(t, actions, "claude", KindHooks)
	if hooks.Change != changeSkipped || !strings.Contains(hooks.Detail, "cannot prove") {
		t.Fatalf("hooks action = %#v", hooks)
	}
	if environment.files[claudeSettings] != userAuthored {
		t.Fatal("a receipt followed the retargeted parent and removed user-authored hooks")
	}
}

// Containment is re-validated immediately before the write, so swapping a
// parent for a symlink after planning cannot redirect it.
func TestApplyRevalidatesTargetBeforeWriting(t *testing.T) {
	environment := guidanceEnvironment()
	options := Options{Targets: []string{"codex"}}
	options.Announce = func([]Action) {
		// Planning has finished; redirect the parent directory.
		environment.symlinks[linuxHome+"/.agents"] = "/srv/repo/instructions"
		environment.dirs["/srv/repo/instructions"] = true
	}

	_, err := Install(context.Background(), environment, grafoPath, options)
	if err == nil || !strings.Contains(err.Error(), "outside the user configuration roots") {
		t.Fatalf("error = %v", err)
	}
	for _, write := range environment.writes {
		if write.path == codexSkill {
			t.Fatalf("wrote after the target was redirected: %v", write)
		}
	}
}

func TestApplyRevalidatesTargetBeforeDeleting(t *testing.T) {
	environment := guidanceEnvironment()
	if _, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	options := Options{Targets: []string{"claude"}}
	options.Announce = func([]Action) {
		environment.symlinks[linuxHome+"/.claude"] = "/srv/repo"
		environment.dirs["/srv/repo"] = true
	}

	_, err := Uninstall(context.Background(), environment, options)
	if err == nil || !strings.Contains(err.Error(), "outside the user configuration roots") {
		t.Fatalf("error = %v", err)
	}
	if len(environment.removes) != 0 {
		t.Fatalf("deleted after the target was redirected: %v", environment.removes)
	}
}

func TestResolvePathHandlesWindowsRoots(t *testing.T) {
	environment := newFakeEnvironment("windows", `C:\Users\u`)
	environment.symlinks[`\\fileserver\profiles\u\.codex`] = `\\fileserver\other\codex`
	environment.symlinks[`C:\Users\u\.codex`] = `C:\Users\u\dotfiles\codex`
	cases := []struct{ path, want string }{
		// A UNC path keeps both leading separators instead of collapsing into a
		// current-drive-rooted path, so its ancestors are inspected at all.
		{`\\fileserver\profiles\u\.codex\AGENTS.md`, `\\fileserver\other\codex\AGENTS.md`},
		{`\\fileserver\share\file.md`, `\\fileserver\share\file.md`},
		{`C:\Users\u\.codex\AGENTS.md`, `C:\Users\u\dotfiles\codex\AGENTS.md`},
		{`C:\Users\u\.claude\settings.json`, `C:\Users\u\.claude\settings.json`},
	}
	for _, test := range cases {
		got, err := resolvePath(environment, test.path)
		if err != nil {
			t.Errorf("resolvePath(%q): %v", test.path, err)
			continue
		}
		if got != test.want {
			t.Errorf("resolvePath(%q) = %q, want %q", test.path, got, test.want)
		}
	}
}

// A symlinked ancestor of a UNC configuration root must be detected, not missed
// because the path was silently rewritten to the current drive.
func TestUNCConfigRootDetectsSymlinkedAncestor(t *testing.T) {
	environment := newFakeEnvironment("windows", `\\fileserver\profiles\u`)
	environment.symlinks[`\\fileserver\profiles\u\.codex`] = `\\fileserver\public\shared`
	environment.dirs[`\\fileserver\public\shared`] = true

	err := checkUserConfigRoot(environment, `\\fileserver\profiles\u\.codex\AGENTS.md`)
	if err == nil || !strings.Contains(err.Error(), "outside the user configuration roots") {
		t.Fatalf("error = %v", err)
	}
	if err = checkUserConfigRoot(environment, `\\fileserver\profiles\u\.claude\skills\grafo\SKILL.md`); err != nil {
		t.Fatalf("a UNC path inside the user root was refused: %v", err)
	}
}

// cmd.exe expands %NAME% even inside double quotes, so such a path cannot be
// quoted safely. Grafo refuses the hook explicitly instead of writing a command
// that would run the wrong binary.
func TestHooksRefuseUnquotableWindowsPath(t *testing.T) {
	environment := newFakeEnvironment("windows", `C:\Users\u`)
	environment.dirs[`C:\Users\u`] = true
	environment.lookups["claude"] = `C:\bin\claude.exe`
	environment.outputs[`C:\bin\claude.exe mcp list`] = ""

	actions, err := Install(context.Background(), environment, `C:\Users\%USERNAME%\grafo.exe`,
		Options{Targets: []string{"claude"}, Hooks: true})
	if err != nil {
		t.Fatal(err)
	}
	hooks := findAction(t, actions, "claude", KindHooks)
	if hooks.Change != changeSkipped {
		t.Fatalf("hooks action = %#v", hooks)
	}
	if !strings.Contains(hooks.Detail, "%") || !strings.Contains(hooks.Detail, "cmd.exe") {
		t.Errorf("diagnostic does not explain the refusal: %q", hooks.Detail)
	}
	if _, exists := environment.files[`C:\Users\u\.claude\settings.json`]; exists {
		t.Fatal("an unquotable hook command was written")
	}
	// The safe artifacts are still installed.
	if got := findAction(t, actions, "claude", KindSkill); got.Change != changeInstalled {
		t.Fatalf("skill action = %#v", got)
	}
}
