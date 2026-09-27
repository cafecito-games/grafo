package indexer

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const gitDirtyPathsMeta = "git_dirty_paths"

type gitChanges struct {
	changed []string
	dirty   []string
}

func detectGitChanges(ctx context.Context, root, indexedCommit string) (gitChanges, error) {
	changed, err := gitPathList(ctx, root, "diff", "--name-only", "-z", indexedCommit, "--")
	if err != nil {
		return gitChanges{}, fmt.Errorf("diff indexed commit: %w", err)
	}
	dirty, err := gitPathList(ctx, root, "diff", "--name-only", "-z", "HEAD", "--")
	if err != nil {
		return gitChanges{}, fmt.Errorf("diff working tree: %w", err)
	}
	untracked, err := gitPathList(ctx, root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return gitChanges{}, fmt.Errorf("list untracked files: %w", err)
	}
	return gitChanges{changed: uniquePaths(changed, untracked), dirty: uniquePaths(dirty, untracked)}, nil
}

func gitPathList(ctx context.Context, root string, arguments ...string) ([]string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, arguments...)...)
	output, err := command.Output()
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
