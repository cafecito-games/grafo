package indexer

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// GitDirtyPathsMeta names the stored ledger of paths that differed from HEAD
// when the index was last written. Together with GitUntrackedPathsMeta it is the
// evidence that lets a refresh scope itself to a precise Git diff; an index
// carrying a commit but no ledger cannot prove which uncommitted files its facts
// came from, so every file is re-read and content-checked instead. Clearing both
// is therefore the supported way to demand a full, content-verified pass.
const GitDirtyPathsMeta = "git_dirty_paths"

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

// gitTracksChangesTo reports whether Git's own diffs account for every change to
// one repository-relative path. Only a path Git holds in its index and still
// compares against the working tree qualifies: an ignored path appears in no
// diff and in no untracked list, and `assume-unchanged` or `skip-worktree` tell
// Git to stop reporting modifications to a path it does track. Change selection
// therefore needs separate proof for all three, and none for an ordinary tracked
// file. The second return value is the number of Git commands spent.
func gitTracksChangesTo(ctx context.Context, root, path string, snapshot *GitSnapshot) (bool, int, error) {
	runner := gitCommandRunner(execGitRunner{})
	if snapshot != nil && snapshot.runner != nil {
		runner = snapshot.runner
	}
	// `ls-files -v` prefixes each record with a status letter and a space. An
	// uppercase "H" is the cached, diff-visible state; the lowercase form marks
	// assume-unchanged and "S" marks skip-worktree.
	records, err := gitPathList(ctx, runner, root, "ls-files", "-v", "-z", "--", path)
	if err != nil {
		return false, 1, err
	}
	return slices.Contains(records, "H "+path), 1, nil
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
