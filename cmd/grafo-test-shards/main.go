// Command grafo-test-shards prints the packages one lane of the CI race test
// run owns, so the workflow can split a step whose wall clock is dominated by a
// single package without maintaining a package list in YAML.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "grafo-test-shards:", err)
		os.Exit(1)
	}
}

func run() error {
	name := flag.String("shard", "", "name of the shard whose packages to print")
	expected := flag.Int("shards", 0, "number of shards the caller was written against")
	flag.Parse()

	if *name == "" {
		return fmt.Errorf("-shard is required")
	}
	// The workflow matrix lists the shards by hand. Comparing the count here
	// makes a shard added to this file and not to the matrix fail every lane
	// instead of quietly going untested.
	if *expected != len(raceShards) {
		return fmt.Errorf("-shards is %d but %d shards are defined; update the workflow matrix", *expected, len(raceShards))
	}

	all, err := listPackages(".")
	if err != nil {
		return err
	}

	assigned, err := partition(raceShards, all)
	if err != nil {
		return err
	}

	packages, defined := assigned[*name]
	if !defined {
		names := make([]string, 0, len(assigned))
		for shardName := range assigned {
			names = append(names, shardName)
		}
		return fmt.Errorf("no shard named %q; defined shards are %s", *name, strings.Join(names, ", "))
	}

	fmt.Println(strings.Join(packages, "\n"))
	return nil
}
