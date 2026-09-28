package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// gitCommandRunner is the narrow process boundary used by repository
// inspection. Tests provide a recorder so command count and exact arguments do
// not depend on wall-clock timings.
type gitCommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execGitRunner struct{}

func (execGitRunner) Run(ctx context.Context, directory string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", directory}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	return command.Output()
}

type cachedGitIdentity struct {
	identity string
	origin   string
	digest   [sha256.Size]byte
}

var gitIdentityCache = struct {
	sync.Mutex
	byRoot map[string]cachedGitIdentity
}{byRoot: map[string]cachedGitIdentity{}}

// GitSnapshot is the typed, machine-readable repository state shared by
// project discovery, membership discovery, and incremental change selection.
// Paths are normalized, deduplicated, and sorted.
type GitSnapshot struct {
	Root             string
	Identity         string
	Branch           string
	Head             string
	Changed          []string
	Dirty            []string
	Untracked        []string
	MembershipStable bool
	DetachedBranch   string
	Commands         int
	ProbeNS          int64
	statusRecords    []gitStatusRecord
	used             *atomic.Bool
	runner           gitCommandRunner
}

type gitStatusRecord struct {
	encoded string
	paths   []string
}

func inspectGit(ctx context.Context, start string, runner gitCommandRunner) (GitSnapshot, bool, error) {
	started := time.Now()
	rootRaw, err := runner.Run(ctx, start, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return GitSnapshot{}, false, err
		}
		return GitSnapshot{}, false, nil
	}
	root := strings.TrimSpace(string(rootRaw))
	if root == "" {
		return GitSnapshot{}, false, fmt.Errorf("git repository root is empty")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return GitSnapshot{}, false, fmt.Errorf("resolve git repository root: %w", err)
	}

	identity, identityCommands, err := inspectGitIdentity(ctx, root, runner)
	if err != nil {
		return GitSnapshot{}, false, err
	}

	statusRaw, err := runner.Run(ctx, root, "status", "--porcelain=v2", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return GitSnapshot{}, false, err
		}
		return GitSnapshot{}, false, fmt.Errorf("inspect git status: %w", err)
	}
	snapshot, err := parseGitStatusPorcelainV2(statusRaw)
	if err != nil {
		return GitSnapshot{}, false, fmt.Errorf("inspect git status: %w", err)
	}
	snapshot.Root = root
	snapshot.Identity = identity
	snapshot.Commands = 2 + identityCommands
	if err := populateDetachedBranch(ctx, root, &snapshot, runner); err != nil {
		return GitSnapshot{}, false, err
	}
	snapshot.ProbeNS = time.Since(started).Nanoseconds()
	snapshot.runner = runner
	return snapshot, true, nil
}

func inspectGitIdentity(ctx context.Context, root string, runner gitCommandRunner) (string, int, error) {
	gitIdentityCache.Lock()
	cached, exists := gitIdentityCache.byRoot[root]
	gitIdentityCache.Unlock()
	if exists {
		if content, err := os.ReadFile(cached.origin); err == nil && sha256.Sum256(content) == cached.digest {
			return cached.identity, 0, nil
		}
	}

	output, commandErr := runner.Run(ctx, root, "config", "--null", "--show-origin", "--get", "remote.origin.url")
	if commandErr != nil {
		if ctx.Err() != nil || errors.Is(commandErr, context.Canceled) || errors.Is(commandErr, context.DeadlineExceeded) {
			return "", 1, commandErr
		}
		cacheGitIdentity(root, root, gitConfigPath(root))
		return root, 1, nil
	}
	parts := bytes.Split(output, []byte{0})
	if len(parts) < 2 {
		return "", 1, fmt.Errorf("git remote identity output is malformed")
	}
	if strings.TrimSpace(string(parts[1])) == "" {
		return root, 1, nil
	}
	identity := strings.TrimSpace(string(parts[1]))
	origin := strings.TrimPrefix(strings.TrimSpace(string(parts[0])), "file:")
	if origin != "" && !filepath.IsAbs(origin) {
		origin = filepath.Join(root, filepath.FromSlash(origin))
	}
	if origin != "" {
		cacheGitIdentity(root, identity, origin)
	}
	return identity, 1, nil
}

func cacheGitIdentity(root, identity, origin string) {
	content, err := os.ReadFile(origin)
	if err != nil {
		return
	}
	gitIdentityCache.Lock()
	gitIdentityCache.byRoot[root] = cachedGitIdentity{identity: identity, origin: origin, digest: sha256.Sum256(content)}
	gitIdentityCache.Unlock()
}

func gitConfigPath(root string) string {
	dotGit := filepath.Join(root, ".git")
	info, err := os.Stat(dotGit)
	if err == nil && info.IsDir() {
		return filepath.Join(dotGit, "config")
	}
	content, err := os.ReadFile(dotGit)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir:") {
		return ""
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	commonDir := gitDir
	if common, readErr := os.ReadFile(filepath.Join(gitDir, "commondir")); readErr == nil {
		commonDir = filepath.Clean(filepath.Join(gitDir, strings.TrimSpace(string(common))))
	}
	return filepath.Join(commonDir, "config")
}

func refreshGitSnapshot(ctx context.Context, previous *GitSnapshot, runner gitCommandRunner) (*GitSnapshot, error) {
	started := time.Now()
	statusRaw, err := runner.Run(ctx, previous.Root, "status", "--porcelain=v2", "-z", "--branch", "--untracked-files=all")
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("inspect git status: %w", err)
	}
	snapshot, err := parseGitStatusPorcelainV2(statusRaw)
	if err != nil {
		return nil, fmt.Errorf("inspect git status: %w", err)
	}
	snapshot.Root = previous.Root
	snapshot.Identity = previous.Identity
	snapshot.Commands = 1
	if err := populateDetachedBranch(ctx, previous.Root, &snapshot, runner); err != nil {
		return nil, err
	}
	snapshot.ProbeNS = time.Since(started).Nanoseconds()
	snapshot.used.Store(true)
	snapshot.runner = runner
	return &snapshot, nil
}

func populateDetachedBranch(ctx context.Context, root string, snapshot *GitSnapshot, runner gitCommandRunner) error {
	if snapshot.Branch != "(detached)" {
		return nil
	}
	shortRaw, err := runner.Run(ctx, root, "rev-parse", "--short=12", "HEAD")
	snapshot.Commands++
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("resolve detached HEAD abbreviation: %w", err)
	}
	abbreviation := strings.TrimSpace(string(shortRaw))
	if abbreviation == "" {
		return fmt.Errorf("resolve detached HEAD abbreviation: empty result")
	}
	snapshot.DetachedBranch = "detached-" + abbreviation
	return nil
}

func parseGitStatusPorcelainV2(raw []byte) (GitSnapshot, error) {
	snapshot := GitSnapshot{MembershipStable: true, used: &atomic.Bool{}}
	tokens := bytes.Split(raw, []byte{0})
	var paths []string
	var untracked []string
	for index := 0; index < len(tokens); index++ {
		token := string(tokens[index])
		if token == "" {
			continue
		}
		// Git currently terminates porcelain headers with LF even in -z mode.
		// Accept NUL-terminated headers too so the parser remains fixture-friendly.
		if strings.HasPrefix(token, "# ") {
			lines := strings.Split(token, "\n")
			for lineIndex, line := range lines {
				if line == "" {
					continue
				}
				if strings.HasPrefix(line, "# ") {
					if err := parseGitStatusHeader(&snapshot, line); err != nil {
						return GitSnapshot{}, err
					}
					continue
				}
				if lineIndex != len(lines)-1 {
					return GitSnapshot{}, fmt.Errorf("malformed porcelain header block")
				}
				token = line
				goto record
			}
			continue
		}
	record:
		switch token[0] {
		case '1':
			fields := strings.SplitN(token, " ", 9)
			if len(fields) != 9 || len(fields[1]) != 2 {
				return GitSnapshot{}, fmt.Errorf("malformed ordinary status record")
			}
			path, err := normalizedGitPath(fields[8])
			if err != nil {
				return GitSnapshot{}, err
			}
			paths = append(paths, path)
			snapshot.statusRecords = append(snapshot.statusRecords, gitStatusRecord{
				encoded: strings.Join(fields[:8], " ") + " " + path, paths: []string{path},
			})
			if membershipStatus(fields[1]) && !PathIgnored(path) {
				snapshot.MembershipStable = false
			}
		case '2':
			fields := strings.SplitN(token, " ", 10)
			if len(fields) != 10 || len(fields[1]) != 2 || index+1 >= len(tokens) || len(tokens[index+1]) == 0 {
				return GitSnapshot{}, fmt.Errorf("malformed rename/copy status record")
			}
			path, err := normalizedGitPath(fields[9])
			if err != nil {
				return GitSnapshot{}, err
			}
			original, err := normalizedGitPath(string(tokens[index+1]))
			if err != nil {
				return GitSnapshot{}, err
			}
			index++
			paths = append(paths, path, original)
			snapshot.statusRecords = append(snapshot.statusRecords, gitStatusRecord{
				encoded: strings.Join(fields[:9], " ") + " " + path + "\x00" + original, paths: []string{path, original},
			})
			if !PathIgnored(path) || !PathIgnored(original) {
				snapshot.MembershipStable = false
			}
		case 'u':
			fields := strings.SplitN(token, " ", 11)
			if len(fields) != 11 {
				return GitSnapshot{}, fmt.Errorf("malformed unmerged status record")
			}
			path, err := normalizedGitPath(fields[10])
			if err != nil {
				return GitSnapshot{}, err
			}
			paths = append(paths, path)
			snapshot.statusRecords = append(snapshot.statusRecords, gitStatusRecord{
				encoded: strings.Join(fields[:10], " ") + " " + path, paths: []string{path},
			})
			if !PathIgnored(path) {
				snapshot.MembershipStable = false
			}
		case '?':
			if !strings.HasPrefix(token, "? ") {
				return GitSnapshot{}, fmt.Errorf("malformed untracked status record")
			}
			path, err := normalizedGitPath(strings.TrimPrefix(token, "? "))
			if err != nil {
				return GitSnapshot{}, err
			}
			paths = append(paths, path)
			snapshot.statusRecords = append(snapshot.statusRecords, gitStatusRecord{encoded: "? " + path, paths: []string{path}})
			if !PathIgnored(path) {
				untracked = append(untracked, path)
			}
		case '!':
			// Ignored records are not requested, but accepting one is safe because
			// ignored paths never participate in selection or membership.
			continue
		default:
			return GitSnapshot{}, fmt.Errorf("unknown porcelain record %q", token[:1])
		}
	}
	if snapshot.Head == "" {
		return GitSnapshot{}, fmt.Errorf("porcelain status omitted branch.oid")
	}
	if snapshot.Branch == "" {
		return GitSnapshot{}, fmt.Errorf("porcelain status omitted branch.head")
	}
	paths = uniquePaths(paths)
	sort.Slice(snapshot.statusRecords, func(i, j int) bool { return snapshot.statusRecords[i].encoded < snapshot.statusRecords[j].encoded })
	snapshot.Changed = append([]string(nil), paths...)
	snapshot.Dirty = append([]string(nil), paths...)
	snapshot.Untracked = uniquePaths(untracked)
	return snapshot, nil
}

func parseGitStatusHeader(snapshot *GitSnapshot, line string) error {
	switch {
	case strings.HasPrefix(line, "# branch.oid "):
		value := strings.TrimSpace(strings.TrimPrefix(line, "# branch.oid "))
		if value == "" {
			return fmt.Errorf("empty branch.oid")
		}
		if value != "(initial)" {
			snapshot.Head = value
		} else {
			snapshot.Head = "(initial)"
		}
	case strings.HasPrefix(line, "# branch.head "):
		value := strings.TrimSpace(strings.TrimPrefix(line, "# branch.head "))
		if value == "" {
			return fmt.Errorf("empty branch.head")
		}
		snapshot.Branch = value
	}
	return nil
}

func membershipStatus(status string) bool {
	return strings.ContainsAny(status, "ADRCT")
}

func normalizedGitPath(value string) (string, error) {
	path := filepath.ToSlash(strings.TrimPrefix(value, "./"))
	if path == "" || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
		return "", fmt.Errorf("invalid git path %q", value)
	}
	return path, nil
}
