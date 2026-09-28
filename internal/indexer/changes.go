package indexer

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

const gitDirtyPathsMeta = "git_dirty_paths"

type gitChanges struct {
	changed     []string
	dirty       []string
	untracked   []string
	gitCommands int
}

func detectGitChanges(ctx context.Context, root, indexedCommit string, snapshot *GitSnapshot) (gitChanges, error) {
	runner := gitCommandRunner(execGitRunner{})
	if snapshot != nil && snapshot.runner != nil {
		runner = snapshot.runner
	}
	if snapshot != nil && snapshot.Root == root && snapshot.Head != "" {
		if indexedCommit == snapshot.Head {
			return gitChanges{changed: append([]string{}, snapshot.Changed...), dirty: append([]string{}, snapshot.Dirty...), untracked: append([]string{}, snapshot.Untracked...)}, nil
		}
		committed, err := gitPathList(ctx, runner, root, "diff", "--name-only", "-z", indexedCommit, snapshot.Head, "--")
		if err != nil {
			return gitChanges{}, fmt.Errorf("diff indexed commit: %w", err)
		}
		return gitChanges{
			changed: uniquePaths(committed, snapshot.Changed),
			dirty:   append([]string{}, snapshot.Dirty...), untracked: append([]string{}, snapshot.Untracked...), gitCommands: 1,
		}, nil
	}
	changed, err := gitPathList(ctx, runner, root, "diff", "--name-only", "-z", indexedCommit, "--")
	if err != nil {
		return gitChanges{}, fmt.Errorf("diff indexed commit: %w", err)
	}
	dirty, err := gitPathList(ctx, runner, root, "diff", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return gitChanges{}, fmt.Errorf("diff working tree: %w", err)
	}
	untracked, err := gitPathList(ctx, runner, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return gitChanges{}, fmt.Errorf("list untracked files: %w", err)
	}
	return gitChanges{changed: uniquePaths(changed, untracked), dirty: uniquePaths(dirty, untracked), untracked: uniquePaths(untracked), gitCommands: 3}, nil
}

func gitPathList(ctx context.Context, runner gitCommandRunner, root string, arguments ...string) ([]string, error) {
	output, err := runner.Run(ctx, root, arguments...)
	if err != nil {
		return nil, err
	}
	result := make([]string, 0)
	for _, value := range bytes.Split(output, []byte{0}) {
		if len(value) == 0 {
			continue
		}
		path := filepath.ToSlash(strings.TrimPrefix(string(value), "./"))
		if path != "" {
			result = append(result, path)
		}
	}
	return result, nil
}

func uniquePaths(groups ...[]string) []string {
	seen := map[string]bool{}
	for _, group := range groups {
		for _, path := range group {
			if PathIgnored(path) {
				continue
			}
			seen[path] = true
		}
	}
	result := make([]string, 0, len(seen))
	for path := range seen {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}
