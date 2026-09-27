package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// toolchainNamespace groups the query commands that only make sense for one
// engine or language under a single parent command, so the language a command
// belongs to is part of its name rather than a prefix convention.
type toolchainNamespace struct {
	// name is the parent command as it is typed.
	name string
	// order lists the subcommands in the order usage reports them, so the
	// message never depends on map iteration.
	order       []string
	subcommands map[string]func(*App, context.Context, parsedArguments) error
}

// toolchainNamespaces holds every parent command. A new language or engine adds
// one entry here and needs no change to the top-level dispatch.
var toolchainNamespaces = map[string]toolchainNamespace{
	"godot": {
		name:  "godot",
		order: []string{"composition", "interactions"},
		subcommands: map[string]func(*App, context.Context, parsedArguments) error{
			"composition":  (*App).godotComposition,
			"interactions": (*App).godotInteractions,
		},
	},
}

func (n toolchainNamespace) usage() string {
	return fmt.Sprintf("usage: grafo %s <%s>", n.name, strings.Join(n.order, "|"))
}

// toolchainCommand routes 'grafo <namespace> <subcommand>' to its handler. The
// namespace and subcommand become the reported command name and the subcommand
// is dropped from the positionals, so each handler sees the same arguments it
// would as a top-level command and reports its own usage under the full name.
func (a *App) toolchainCommand(ctx context.Context, namespace toolchainNamespace, args parsedArguments) error {
	subcommand := ""
	rest := args.positionals
	if len(rest) > 0 {
		subcommand, rest = rest[0], rest[1:]
	}
	handler, known := namespace.subcommands[subcommand]
	if !known {
		if subcommand == "" {
			return errors.New(namespace.usage())
		}
		return fmt.Errorf("unknown %s subcommand %q (%s)", namespace.name, subcommand, namespace.usage())
	}
	args.command += " " + subcommand
	args.positionals = rest
	return handler(a, ctx, args)
}
