// Package indexseed bootstraps a worktree's branch index by adopting a
// compatible index from a sibling worktree of the same repository.
//
// A new worktree starts with no index and pays a full parse of every file, even
// when the checkout next to it already holds a complete index of nearly the same
// content. That index is already addressable by this worktree: node and fact
// identifiers hash the repository identity and repository-relative paths, never
// an absolute location, so a sibling's graph describes the same repository rather
// than the same directory.
//
// Adoption is only ever an optimization. It copies the sibling's database, fixes
// the two metadata values that describe a location rather than a repository, and
// clears the dirty ledgers so the following indexing pass re-reads and re-hashes
// every file it discovers. Nothing from the donor survives unless a
// byte-identical file exists in this worktree, which is what makes a donor's
// uncommitted edits structurally unable to leak. Every failure here is reported
// as a reason and leaves the caller to index from scratch exactly as before.
package indexseed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/projectconfig"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
)

// inspectionTimeout bounds the read of one candidate index. A donor that cannot
// describe itself quickly is not worth waiting for when the fallback is the
// indexing pass the caller was going to run anyway.
const inspectionTimeout = 2 * time.Second

// rankedCandidateLimit bounds how many candidates have their divergence measured
// with Git. Discovery cost must not grow with the number of worktrees a
// long-running agent workflow leaves behind.
const rankedCandidateLimit = 4

// Unlock releases a lock held over a donor index.
type Unlock func() error

// Options supplies the two capabilities adoption cannot own itself.
type Options struct {
	// TryLock takes the caller's index write lock on a donor without waiting, so
	// an index another process is refreshing is skipped rather than copied
	// mid-write. A nil TryLock disables adoption, because copying an unlocked
	// donor cannot be proven safe.
	TryLock func(indexPath string) (Unlock, bool, error)
	// Git runs Git in a directory. Nil selects the real command.
	Git GitRunner
}

// GitRunner is the process boundary used to enumerate worktrees and measure
// divergence, so tests can assert the exact commands rather than depend on a
// fixture repository's history.
type GitRunner interface {
	Run(ctx context.Context, directory string, arguments ...string) ([]byte, error)
}

type execGitRunner struct{}

func (execGitRunner) Run(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	return indexer.RunGit(ctx, directory, arguments...)
}

// Result reports what adoption did, or why it declined. Declining is an ordinary
// outcome: Seeded is false and Reason explains it in terms a user can act on.
type Result struct {
	Seeded bool `json:"seeded"`
	// Reason is empty exactly when Seeded is true.
	Reason     string                  `json:"reason,omitempty"`
	Provenance *indexer.SeedProvenance `json:"provenance,omitempty"`
	// Considered counts the candidate indexes that were inspected, so a decline
	// distinguishes "no sibling had one" from "none of several was usable".
	Considered int `json:"considered"`
}

// candidate is one sibling index that passed inspection.
type candidate struct {
	indexPath    string
	donorRoot    string
	branch       string
	commit       string
	indexedAt    string
	changedPaths int
}

// Seed adopts the best available sibling index for project, writing it to
// project.IndexPath. The caller must already hold project's own index write
// lock, and must not call Seed when that index exists.
//
// Only a context error is returned as an error. Everything else, including a
// failed copy, is a declined Result so the caller proceeds to a normal index.
func Seed(ctx context.Context, project indexer.Project, options Options) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if options.TryLock == nil {
		return Result{Reason: "no donor lock was supplied"}, nil
	}
	if configuration, err := projectconfig.Load(project.Root); err == nil {
		if configuration.IndexSeed != nil && !*configuration.IndexSeed {
			return Result{Reason: "index.seed is disabled in " + projectconfig.FileName}, nil
		}
	}
	if !project.GitManaged {
		return Result{Reason: "project is not Git-managed, so it has no sibling worktrees"}, nil
	}
	if project.Commit == "" {
		return Result{Reason: "project has no commit to compare a donor against"}, nil
	}
	if _, err := os.Lstat(project.IndexPath); err == nil {
		return Result{Reason: "an index for this branch already exists"}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{Reason: fmt.Sprintf("inspect index path: %v", err)}, nil
	}
	git := options.Git
	if git == nil {
		git = execGitRunner{}
	}
	siblings, err := siblingWorktrees(ctx, git, project.Root)
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{Reason: fmt.Sprintf("list worktrees: %v", err)}, nil
	}
	if len(siblings) == 0 {
		return Result{Reason: "no sibling worktree exists"}, nil
	}
	candidates, err := inspectCandidates(ctx, project, siblings)
	if err != nil {
		return Result{}, err
	}
	if len(candidates) == 0 {
		return Result{Reason: "no sibling worktree holds a compatible index of this repository"}, nil
	}
	ranked := rank(ctx, git, project, candidates)
	for _, chosen := range ranked {
		result, adopted, err := adopt(ctx, project, chosen, options.TryLock)
		if err != nil {
			return Result{}, err
		}
		if adopted {
			result.Considered = len(candidates)
			return result, nil
		}
	}
	return Result{
		Reason:     "every compatible donor was locked or could not be copied",
		Considered: len(candidates),
	}, nil
}

// siblingWorktrees returns the canonical roots of every worktree of project's
// repository except project's own. The main checkout is included: `git worktree
// list` reads the repository's common directory, so it reports the same set from
// inside any linked worktree, with the main checkout first.
func siblingWorktrees(ctx context.Context, git GitRunner, root string) ([]string, error) {
	output, err := git.Run(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	roots := make([]string, 0, 4)
	for _, line := range strings.Split(string(output), "\n") {
		path, found := strings.CutPrefix(strings.TrimSpace(line), "worktree ")
		if !found || path == "" {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			// A worktree whose directory is gone is listed until it is pruned.
			continue
		}
		if resolved == root {
			continue
		}
		roots = append(roots, resolved)
	}
	sort.Strings(roots)
	return roots, nil
}

// inspectCandidates reads every index each sibling holds and keeps the ones that
// describe this repository and are compatible with this build. Compatibility
// inspection already proves the schema and semantic index versions, so a
// compatible candidate cannot be one the indexer would rebuild.
func inspectCandidates(ctx context.Context, project indexer.Project, siblings []string) ([]candidate, error) {
	candidates := make([]candidate, 0, len(siblings))
	for _, sibling := range siblings {
		matches, err := filepath.Glob(filepath.Join(sibling, ".grafo", "indexes", "*.sqlite"))
		if err != nil {
			continue
		}
		sort.Strings(matches)
		for _, match := range matches {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			info, err := os.Lstat(match)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			bounded, cancel := context.WithTimeout(ctx, inspectionTimeout)
			inspection, err := sqlite.InspectIndex(bounded, match)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue
			}
			if inspection.Compatibility != sqlite.CompatibilityCompatible {
				continue
			}
			if inspection.Metadata.RepositoryID != project.ID || inspection.Metadata.Commit == "" {
				continue
			}
			candidates = append(candidates, candidate{
				indexPath: match, donorRoot: sibling,
				branch:    inspection.Metadata.Branch,
				commit:    inspection.Metadata.Commit,
				indexedAt: inspection.Metadata.IndexedAt,
			})
		}
	}
	return candidates, nil
}

// rank orders candidates by how much of their content this worktree can keep.
//
// A donor at this worktree's commit is trivially best. Otherwise the count of
// tracked files whose blobs differ between the two commits is the exact
// complement of the files the adopting pass can reuse, so it is measured
// directly with a two-dot diff. Ancestry is irrelevant: a two-dot diff reports
// files that differ in either direction, which is precisely the re-read set.
//
// Measuring is bounded to the most recently indexed candidates; the rest keep
// their recency order behind the measured ones.
func rank(ctx context.Context, git GitRunner, project indexer.Project, candidates []candidate) []candidate {
	ordered := append([]candidate(nil), candidates...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].indexedAt != ordered[right].indexedAt {
			return ordered[left].indexedAt > ordered[right].indexedAt
		}
		return ordered[left].indexPath < ordered[right].indexPath
	})
	measured := make([]candidate, 0, len(ordered))
	unmeasured := make([]candidate, 0, len(ordered))
	for _, item := range ordered {
		if item.commit == project.Commit {
			item.changedPaths = 0
			measured = append(measured, item)
			continue
		}
		if len(measured) >= rankedCandidateLimit {
			item.changedPaths = -1
			unmeasured = append(unmeasured, item)
			continue
		}
		changed, err := changedPathCount(ctx, git, project.Root, item.commit, project.Commit)
		if err != nil {
			// A commit this worktree cannot resolve cannot be compared, and a
			// donor whose divergence is unknown is worse than one that is known.
			item.changedPaths = -1
			unmeasured = append(unmeasured, item)
			continue
		}
		item.changedPaths = changed
		measured = append(measured, item)
	}
	sort.SliceStable(measured, func(left, right int) bool {
		return measured[left].changedPaths < measured[right].changedPaths
	})
	return append(measured, unmeasured...)
}

// changedPathCount counts the tracked files whose content differs between two
// commits.
func changedPathCount(ctx context.Context, git GitRunner, root, from, to string) (int, error) {
	output, err := git.Run(ctx, root, "diff", "--name-only", "-z", from, to, "--")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, value := range strings.Split(string(output), "\x00") {
		if value != "" {
			count++
		}
	}
	return count, nil
}

// adopt copies one candidate into place under the donor's own write lock. The
// copy is published by rename so a crash can never leave a partial index where a
// complete one is expected, and the metadata correction happens before the
// rename so the published file already describes this worktree.
//
// A false adopted result means this candidate was unusable and the next should
// be tried.
func adopt(ctx context.Context, project indexer.Project, chosen candidate,
	tryLock func(string) (Unlock, bool, error)) (Result, bool, error) {
	unlock, acquired, err := tryLock(chosen.indexPath)
	if err != nil || !acquired {
		return Result{}, false, nil
	}
	defer func() { _ = unlock() }()

	if err := os.MkdirAll(filepath.Dir(project.IndexPath), 0o755); err != nil {
		return Result{}, false, nil
	}
	temporary := project.IndexPath + ".seed-" + fmt.Sprint(os.Getpid())
	removeTemporary := func() {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(temporary + suffix)
		}
	}
	removeTemporary()
	if err := sqlite.CopyIndex(ctx, chosen.indexPath, temporary); err != nil {
		removeTemporary()
		if ctx.Err() != nil {
			return Result{}, false, ctx.Err()
		}
		return Result{}, false, nil
	}
	// root and branch are the only stored values that describe where an index
	// lives rather than what it contains. The repository node carries the donor's
	// pair too, but the workspace digest covers those properties, so the
	// adopting pass replaces that node on its own. Clearing both dirty ledgers is
	// what forces that pass to re-read every file instead of trusting a Git diff
	// against the donor's uncommitted state. The freshness token is cleared for a
	// different reason: the ledgers keep this pass from reusing a file's facts it
	// cannot prove, while the token keeps a reader from being convinced the index
	// is already current. It names the donor's inputs, so an adopter that kept it
	// could match a probe and be served without ever being reconciled.
	if err := sqlite.SetIndexMeta(ctx, temporary, map[string]string{
		"root":                        project.Root,
		"branch":                      project.Branch,
		indexer.GitDirtyPathsMeta:     "",
		indexer.GitUntrackedPathsMeta: "",
		indexer.FreshnessTokenMeta:    "",
	}); err != nil {
		removeTemporary()
		if ctx.Err() != nil {
			return Result{}, false, ctx.Err()
		}
		return Result{}, false, nil
	}
	if err := os.Rename(temporary, project.IndexPath); err != nil {
		removeTemporary()
		return Result{}, false, nil
	}
	// Sidecars belong to the temporary name and describe a checkpointed copy, so
	// leaving them would strand files no connection will ever adopt.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(temporary + suffix)
	}
	return Result{Seeded: true, Provenance: &indexer.SeedProvenance{
		DonorRoot: chosen.donorRoot, DonorBranch: chosen.branch,
		DonorCommit: chosen.commit, ChangedPaths: chosen.changedPaths,
	}}, true, nil
}
