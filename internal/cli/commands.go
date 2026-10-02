package cli

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// commandHandler is the shape every top-level command shares once its extra
// arguments are bound, so aliases and namespaces route through one table.
type commandHandler func(*App, context.Context, parsedArguments) error

// commandEntry binds one command to its handler. The first name is the
// canonical spelling offered in suggestions; the remaining names are accepted
// aliases that stay out of the suggestions so a single command is never
// proposed under three spellings.
type commandEntry struct {
	names   []string
	handler commandHandler
}

// commandTable is the single registration point for top-level commands. A new
// command adds one entry and becomes both dispatchable and suggestible.
var commandTable = []commandEntry{
	{names: []string{"install"}, handler: (*App).install},
	{names: []string{"uninstall"}, handler: (*App).uninstall},
	{names: []string{"guidance"}, handler: (*App).guidance},
	{names: []string{"index"}, handler: (*App).index},
	{names: []string{"indexes"}, handler: (*App).indexes},
	{names: []string{"watch"}, handler: (*App).watch},
	{names: []string{"service"}, handler: (*App).service},
	{names: []string{"doctor"}, handler: (*App).doctor},
	{names: []string{"status", "counts"}, handler: (*App).status},
	{names: []string{"mcp"}, handler: (*App).mcp},
	{names: []string{"embed"}, handler: (*App).embed},
	{names: []string{"embed-cache"}, handler: (*App).embedCache},
	{names: []string{"reusable", "find-reusable-code"}, handler: (*App).reusable},
	{names: []string{"find"}, handler: (*App).find},
	{names: []string{"show"}, handler: (*App).show},
	{names: []string{"source"}, handler: (*App).source},
	{names: []string{"neighbors", "query"}, handler: neighborsHandler("")},
	{names: []string{"callers"}, handler: neighborsHandler("callers")},
	{names: []string{"callees"}, handler: neighborsHandler("callees")},
	{names: []string{"impact", "blast-radius"}, handler: (*App).impact},
	{names: []string{"failure-flow", "get-failure-flow"}, handler: (*App).failureFlow},
	{names: []string{"search"}, handler: (*App).search},
	{names: []string{"path"}, handler: (*App).path},
	{names: []string{"data-resources"}, handler: (*App).dataResources},
	{names: []string{"data-usage"}, handler: (*App).dataResourceUsage},
	{names: []string{"config-keys"}, handler: (*App).configKeys},
	{names: []string{"events"}, handler: (*App).events},
	{names: []string{"orphaned-events"}, handler: (*App).orphanedEvents},
	{names: []string{"endpoints", "list-endpoints", "list_endpoints"}, handler: (*App).endpoints},
	{names: []string{"outbound-requests", "list-outbound-requests", "list_outbound_requests"}, handler: (*App).outboundRequests},
	{names: []string{"find-handler", "find_handler"}, handler: (*App).findHandler},
	{names: []string{"service-topology", "get-service-topology", "get_service_topology"}, handler: (*App).serviceTopology},
	{names: []string{"message-flow", "get-message-flow", "get_message_flow"}, handler: (*App).messageFlow},
	{names: []string{"message-coverage", "list-message-coverage", "list_message_coverage"}, handler: (*App).messageCoverage},
	{names: []string{"find-tests", "find_tests"}, handler: testCoverageHandler(true)},
	{names: []string{"test-coverage", "get-test-coverage", "get_test_coverage"}, handler: testCoverageHandler(false)},
}

// builtinCommands are handled before the dispatch table but are still part of
// the vocabulary, so a misspelling of either is corrected like any other.
var builtinCommands = []string{"help", "version"}

var commandHandlers, commandVocabulary = indexCommands()

func neighborsHandler(relation string) commandHandler {
	return func(a *App, ctx context.Context, args parsedArguments) error {
		return a.neighbors(ctx, args, relation)
	}
}

func testCoverageHandler(testsOnly bool) commandHandler {
	return func(a *App, ctx context.Context, args parsedArguments) error {
		return a.testCoverage(ctx, args, testsOnly)
	}
}

// indexCommands flattens the dispatch table into the lookup used on every run
// and the sorted canonical vocabulary used for suggestions.
func indexCommands() (map[string]commandHandler, []string) {
	handlers := make(map[string]commandHandler)
	vocabulary := slices.Clone(builtinCommands)
	for _, entry := range commandTable {
		for _, name := range entry.names {
			handlers[name] = entry.handler
		}
		vocabulary = append(vocabulary, entry.names[0])
	}
	for namespace := range toolchainNamespaces {
		vocabulary = append(vocabulary, namespace)
	}
	sort.Strings(vocabulary)
	return handlers, vocabulary
}

// maximumSuggestions keeps the error to a glance even when a command is close
// to many others.
const maximumSuggestions = 3

// minimumSubstringLength stops a one or two character command from matching
// every name that happens to contain those letters.
const minimumSubstringLength = 3

// unknownCommandError reports an unrecognized command and, when one is close
// enough to be the intent, names the real spelling. Pointing at the command
// costs a line here and saves a caller from concluding the capability is
// missing when only the name was wrong.
func unknownCommandError(command string) error {
	suggestions := suggestCommands(command)
	if len(suggestions) == 0 {
		return fmt.Errorf("unknown command %q (run 'grafo help')", command)
	}
	quoted := make([]string, 0, len(suggestions))
	for _, suggestion := range suggestions {
		quoted = append(quoted, fmt.Sprintf("%q", suggestion))
	}
	alternatives := quoted[0]
	if len(quoted) > 1 {
		alternatives = strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
	}
	return fmt.Errorf("unknown command %q (did you mean %s? run 'grafo help')", command, alternatives)
}

// suggestCommands ranks canonical command names by closeness to an
// unrecognized one. A shared substring ranks ahead of every edit-distance
// match because the common miss is a partial name rather than a typo: a caller
// reaching for the embedding cache writes "cache", not a misspelling of
// "embed-cache". Ties break alphabetically so the error is reproducible.
func suggestCommands(command string) []string {
	needle := strings.ToLower(strings.TrimSpace(command))
	if needle == "" {
		return nil
	}
	type scored struct {
		name  string
		score int
	}
	var matches []scored
	for _, name := range commandVocabulary {
		if name == needle {
			continue
		}
		if len(needle) >= minimumSubstringLength && (strings.Contains(name, needle) || strings.Contains(needle, name)) {
			matches = append(matches, scored{name: name, score: 0})
			continue
		}
		distance := editDistance(needle, name)
		if distance <= distanceBudget(needle) {
			matches = append(matches, scored{name: name, score: distance})
		}
	}
	slices.SortFunc(matches, func(a, b scored) int {
		if a.score != b.score {
			return a.score - b.score
		}
		return strings.Compare(a.name, b.name)
	})
	if len(matches) > maximumSuggestions {
		matches = matches[:maximumSuggestions]
	}
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, match.name)
	}
	return names
}

// distanceBudget scales the tolerated number of edits with the length of what
// was typed, so a short command is not corrected to an unrelated one.
func distanceBudget(command string) int {
	switch {
	case len(command) <= 4:
		return 1
	case len(command) <= 8:
		return 2
	default:
		return 3
	}
}

// editDistance is the Levenshtein distance between two command names, over
// bytes because every command name is ASCII.
func editDistance(left, right string) int {
	previous := make([]int, len(right)+1)
	current := make([]int, len(right)+1)
	for column := range previous {
		previous[column] = column
	}
	for row := 1; row <= len(left); row++ {
		current[0] = row
		for column := 1; column <= len(right); column++ {
			substitution := previous[column-1]
			if left[row-1] != right[column-1] {
				substitution++
			}
			current[column] = min(substitution, min(previous[column]+1, current[column-1]+1))
		}
		previous, current = current, previous
	}
	return previous[len(right)]
}
