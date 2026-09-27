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
	codexAgents    = linuxHome + "/.codex/AGENTS.md"
)

// guidanceEnvironment returns an environment where Claude Code and Codex are the
// only detected clients, so guidance covers a skill file and a managed block.
func guidanceEnvironment() *fakeEnvironment {
	environment := cliEnvironment(map[string]string{"claude": "/bin/claude", "codex": "/bin/codex"})
	environment.outputs["/bin/claude mcp list"] = ""
	environment.outputs["/bin/codex mcp list"] = ""
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

func TestInstallWritesSkillAndManagedBlock(t *testing.T) {
	environment := guidanceEnvironment()
	environment.files[codexAgents] = "# My rules\n\nAlways run the tests.\n"

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

	block := findAction(t, actions, "codex", KindInstructions)
	if block.Target != codexAgents || block.Change != changeInstalled {
		t.Fatalf("instructions action = %#v", block)
	}
	merged := environment.files[codexAgents]
	if !strings.HasPrefix(merged, "# My rules\n\nAlways run the tests.\n") {
		t.Fatalf("user content was not preserved: %q", merged)
	}
	if strings.Count(merged, agentguide.BeginMarker) != 1 {
		t.Fatalf("managed block count is wrong: %q", merged)
	}
	if !slicesContainsPath(environment.mkdirs, linuxHome+"/.claude/skills/grafo") {
		t.Fatalf("mkdirs = %v", environment.mkdirs)
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
		if write.path == claudeSkill || write.path == codexAgents {
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
	environment.files[codexAgents] = "# My rules\n"
	before := environment.files[codexAgents]
	environment.strict = true

	actions, err := Install(context.Background(), environment, grafoPath, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	environment.assertNoMutations(t)
	if environment.files[codexAgents] != before {
		t.Fatal("dry run changed the instruction file")
	}
	if _, exists := environment.files[claudeSkill]; exists {
		t.Fatal("dry run created the skill file")
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
	if _, exists := environment.files[codexAgents]; exists {
		t.Fatal("--mcp-only created an instruction file")
	}
}

func TestRefreshUpdatesOnlyExistingArtifacts(t *testing.T) {
	environment := guidanceEnvironment()
	// A stale Grafo-owned skill exists; the Codex block does not.
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
	block := findAction(t, actions, "codex", KindInstructions)
	if block.Change != changeSkipped {
		t.Fatalf("refresh installed a missing artifact: %#v", block)
	}
	if _, exists := environment.files[codexAgents]; exists {
		t.Fatal("refresh created an instruction file")
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
			environment := guidanceEnvironment()
			environment.files[codexAgents] = contents

			_, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"codex"}})
			if err == nil || !strings.Contains(err.Error(), "grafo-guidance markers are") {
				t.Fatalf("error = %v", err)
			}
			if environment.files[codexAgents] != contents {
				t.Fatal("the conflicting file was rewritten")
			}
		})
	}
}

func TestInstallRefusesUnsafeGuidanceTargets(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		environment := guidanceEnvironment()
		environment.symlinks[codexAgents] = "/etc/passwd"

		_, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"codex"}})
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("error = %v", err)
		}
		for _, write := range environment.writes {
			if write.path == codexAgents {
				t.Fatalf("wrote through the symlink: %v", write)
			}
		}
	})
	t.Run("world writable", func(t *testing.T) {
		environment := guidanceEnvironment()
		environment.files[codexAgents] = "# rules\n"
		environment.modes[codexAgents] = 0o666

		_, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"codex"}})
		if err == nil || !strings.Contains(err.Error(), "world-writable") {
			t.Fatalf("error = %v", err)
		}
	})
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
	environment := guidanceEnvironment()
	environment.files[codexAgents] = "# rules\n"
	environment.modes[codexAgents] = 0o600

	if _, err := Install(context.Background(), environment, grafoPath, Options{Targets: []string{"codex"}}); err != nil {
		t.Fatal(err)
	}
	for _, write := range environment.writes {
		if write.path == codexAgents && write.perm != fs.FileMode(0o600) {
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
	if got := findAction(t, actions, "codex", KindInstructions); got.Change != changeRemoved {
		t.Fatalf("instructions action = %#v", got)
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
		if !strings.Contains(settings, HookCommand(grafoPath, phase.phase)) {
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
      },
      {
        "matcher": "Grep",
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
	options := Options{Targets: []string{"claude"}, Hooks: true}
	actions, err := Install(context.Background(), environment, grafoPath, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := findAction(t, actions, "claude", KindHooks); got.Change != changeUpdated {
		t.Fatalf("hooks action = %#v", got)
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
	actions, err = Install(context.Background(), environment, grafoPath, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := findAction(t, actions, "claude", KindHooks); got.Change != changeUnchanged {
		t.Fatalf("replayed hooks action = %#v", got)
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

func TestGrafoHookCommandOwnership(t *testing.T) {
	cases := map[string]bool{
		"/opt/bin/grafo guidance --hook pre-search": true,
		"grafo.exe guidance --hook pre-edit":        true,
		"/opt/bin/grafo guidance --hook other":      false,
		"/opt/bin/grafo mcp":                        false,
		"/usr/local/bin/audit":                      false,
		"other guidance --hook pre-edit":            false,
	}
	for command, want := range cases {
		if got := grafoHookCommand(command); got != want {
			t.Errorf("grafoHookCommand(%q) = %v, want %v", command, got, want)
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
