package indexer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

type Project struct {
	Root        string `json:"root"`
	Name        string `json:"name"`
	ID          string `json:"id"`
	Branch      string `json:"branch"`
	Commit      string `json:"commit,omitempty"`
	GoModule    string `json:"go_module,omitempty"`
	IndexPath   string `json:"index_path"`
	GitManaged  bool   `json:"git_managed"`
	gitSnapshot *GitSnapshot
}

var safeNamePattern = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func DiscoverProject(ctx context.Context, start string) (Project, error) {
	return discoverProject(ctx, start, execGitRunner{})
}

func discoverProject(ctx context.Context, start string, runner gitCommandRunner) (Project, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return Project{}, err
	}
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project root: %w", err)
	}
	snapshot, gitManaged, err := inspectGit(ctx, root, runner)
	if err != nil {
		return Project{}, err
	}
	if gitManaged {
		root = snapshot.Root
	}
	name := filepath.Base(root)
	identity := root
	branch := "working-tree"
	commit := ""
	if gitManaged {
		identity = snapshot.Identity
		branch = snapshot.Branch
		commit = snapshot.Head
		if branch == "(detached)" {
			branch = snapshot.DetachedBranch
		}
		if commit == "(initial)" {
			commit = ""
		}
	}
	branchHash := sha256.Sum256([]byte(branch))
	safeBranch := strings.Trim(safeNamePattern.ReplaceAllString(branch, "-"), "-")
	if safeBranch == "" {
		safeBranch = "branch"
	}
	if len(safeBranch) > 48 {
		safeBranch = safeBranch[:48]
	}
	indexName := safeBranch + "-" + hex.EncodeToString(branchHash[:])[:10] + ".sqlite"
	var projectSnapshot *GitSnapshot
	if gitManaged {
		projectSnapshot = &snapshot
	}
	return Project{
		Root: root, Name: name, ID: graph.StableID("repo", identity), Branch: branch,
		Commit: commit, GoModule: readGoModule(root), gitSnapshot: projectSnapshot,
		IndexPath: filepath.Join(root, ".grafo", "indexes", indexName), GitManaged: gitManaged,
	}, nil
}

func readGoModule(root string) string {
	content, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return ""
}
