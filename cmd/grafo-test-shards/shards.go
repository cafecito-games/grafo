package main

import (
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// shard is one lane of the race test run. Packages names the packages the lane
// owns; a lane that names none takes the complement, so a package added to the
// repository is tested without anyone remembering to assign it.
type shard struct {
	Name     string
	Packages []string
}

// raceShards splits the run so no lane is much longer than the longest single
// package, which is as short as package-level sharding can make the step.
// go test runs GOMAXPROCS packages at a time inside a lane, so a lane's own
// wall clock is roughly its total work over the runner's 4 cores but never less
// than its slowest package.
//
// internal/eval, internal/federation and internal/cli run serially, and
// internal/indexer is the longest package even with its tests parallel, so
// those take the named lanes. Everything else, including whatever lands in the
// repository next, shares the last one. The lane that absorbs new packages is
// deliberately one of the shortest, so a package added tomorrow does not
// immediately become the step's floor.
var raceShards = []shard{
	{Name: "eval", Packages: []string{
		"github.com/cafecito-games/grafo/internal/eval",
	}},
	{Name: "federation", Packages: []string{
		"github.com/cafecito-games/grafo/internal/federation",
		"github.com/cafecito-games/grafo/internal/cli",
	}},
	{Name: "indexer", Packages: []string{
		"github.com/cafecito-games/grafo/internal/indexer",
	}},
	{Name: "rest"},
}

// partition assigns every package in all to exactly one shard. It fails rather
// than guessing, because a shard definition that silently stops covering a
// package turns an untested package into a passing run.
func partition(shards []shard, all []string) (map[string][]string, error) {
	owner := make(map[string]string, len(all))
	complement := ""
	assigned := make(map[string][]string, len(shards))

	for _, candidate := range shards {
		if _, exists := assigned[candidate.Name]; exists {
			return nil, fmt.Errorf("duplicate shard name %q", candidate.Name)
		}
		assigned[candidate.Name] = nil

		if len(candidate.Packages) == 0 {
			if complement != "" {
				return nil, fmt.Errorf(
					"exactly one shard must claim no packages so new packages are still tested; %q and %q both claim none",
					complement, candidate.Name)
			}
			complement = candidate.Name
			continue
		}

		for _, name := range candidate.Packages {
			if !slices.Contains(all, name) {
				return nil, fmt.Errorf(
					"shard %q claims %s, which the module no longer contains; update the shard definition",
					candidate.Name, name)
			}
			if previous, taken := owner[name]; taken {
				return nil, fmt.Errorf("%s is claimed by both shard %q and shard %q", name, previous, candidate.Name)
			}
			owner[name] = candidate.Name
			assigned[candidate.Name] = append(assigned[candidate.Name], name)
		}
	}

	if complement == "" {
		return nil, fmt.Errorf("exactly one shard must claim no packages so new packages are still tested; none does")
	}

	for _, name := range all {
		if _, taken := owner[name]; !taken {
			assigned[complement] = append(assigned[complement], name)
		}
	}

	return assigned, nil
}

// listPackages reports the module's packages in the order go list emits them.
func listPackages(dir string) ([]string, error) {
	command := exec.Command("go", "list", "./...")
	command.Dir = dir

	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./...: %w", err)
	}

	var packages []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			packages = append(packages, line)
		}
	}
	return packages, nil
}
