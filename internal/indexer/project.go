package indexer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cafecito-games/grafo/internal/graph"
)

type Project struct {
	Root       string `json:"root"`
	Name       string `json:"name"`
	ID         string `json:"id"`
	Branch     string `json:"branch"`
	Commit     string `json:"commit,omitempty"`
	GoModule   string `json:"go_module,omitempty"`
	IndexPath  string `json:"index_path"`
	GitManaged bool   `json:"git_managed"`
}

var safeNamePattern = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func DiscoverProject(ctx context.Context, start string) (Project, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return Project{}, err
	}
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	root := abs
	gitManaged := false
	if value, err := git(ctx, abs, "rev-parse", "--show-toplevel"); err == nil && value != "" {
		root = value
		gitManaged = true
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return Project{}, fmt.Errorf("resolve project root: %w", err)
	}
	name := filepath.Base(root)
	identity := root
	branch := "working-tree"
	commit := ""
	if gitManaged {
		if remote, err := git(ctx, root, "config", "--get", "remote.origin.url"); err == nil && remote != "" {
			identity = remote
		}
		if value, err := git(ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && value != "" {
			branch = value
		} else if value, err := git(ctx, root, "rev-parse", "--short=12", "HEAD"); err == nil && value != "" {
			branch = "detached-" + value
		}
		commit, _ = git(ctx, root, "rev-parse", "HEAD")
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
	return Project{
		Root: root, Name: name, ID: graph.StableID("repo", identity), Branch: branch,
		Commit: commit, GoModule: readGoModule(root),
		IndexPath: filepath.Join(root, ".grafo", "indexes", indexName), GitManaged: gitManaged,
	}, nil
}

func git(ctx context.Context, directory string, args ...string) (string, error) {
	commandArgs := append([]string{"-C", directory}, args...)
	command := exec.CommandContext(ctx, "git", commandArgs...)
	output, err := command.Output()
	return strings.TrimSpace(string(output)), err
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
