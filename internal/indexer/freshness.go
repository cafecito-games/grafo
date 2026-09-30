package indexer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	parserapi "github.com/cafecito-games/grafo/internal/parser"
	"github.com/cafecito-games/grafo/internal/projectconfig"
	"github.com/cafecito-games/grafo/internal/repositorypath"
)

const (
	// FreshnessTokenVersion identifies the inputs and encoding used by
	// ProbeFreshness. Tokens from different versions never compare equal.
	FreshnessTokenVersion = "freshness-v1"
	defaultFreshnessMax   = int64(5 << 20)
)

// FreshnessOptions bounds content inspection in the same way as indexing.
type FreshnessOptions struct {
	MaxFileSize int64
}

// FreshnessToken is an opaque, versioned description of every input which can
// affect the persisted index for one Git worktree. Callers may compare and log
// tokens, but must not interpret their representation.
type FreshnessToken struct {
	version string
	digest  [sha256.Size]byte
}

// Version reports the token contract version.
func (t FreshnessToken) Version() string { return t.version }

// Equal reports whether two tokens describe the same indexed inputs.
func (t FreshnessToken) Equal(other FreshnessToken) bool {
	return t.version != "" && t.version == other.version &&
		subtle.ConstantTimeCompare(t.digest[:], other.digest[:]) == 1
}

// String returns an opaque stable identifier suitable for generation labels.
func (t FreshnessToken) String() string {
	if t.version == "" {
		return ""
	}
	return t.version + ":" + hex.EncodeToString(t.digest[:])
}

// FreshnessProbe combines a discovered project with its freshness evidence.
// Unsupported means the project is not Git-managed and must conservatively be
// refreshed for every request generation.
type FreshnessProbe struct {
	Project                   Project
	Token                     FreshnessToken
	Supported                 bool
	Fallback                  string
	GitCommands               int
	workspaceSemanticKeys     map[string]string
	workspaceSemanticEvidence map[string]string
	hiddenSemanticDigest      [sha256.Size]byte
}

// ProbeFreshness computes a stable token from Git state, dirty/untracked
// parser inputs, validated repository configuration, and parser semantic keys.
func ProbeFreshness(ctx context.Context, start string, registry *parserapi.Registry, options FreshnessOptions) (FreshnessProbe, error) {
	if registry == nil {
		return FreshnessProbe{}, fmt.Errorf("freshness parser registry is required")
	}
	if err := ctx.Err(); err != nil {
		return FreshnessProbe{}, err
	}
	project, err := DiscoverProject(ctx, start)
	if err != nil {
		return FreshnessProbe{}, err
	}
	return probeProjectFreshness(ctx, project, registry, options, nil, false)
}

// ReprobeFreshness updates a prior probe with one porcelain status command in
// the steady state. Canonical root and cached remote identity are revalidated;
// branch, HEAD, and every status record come from the new snapshot.
func ReprobeFreshness(ctx context.Context, previous FreshnessProbe, registry *parserapi.Registry, options FreshnessOptions) (FreshnessProbe, error) {
	if !previous.Project.GitManaged || previous.Project.gitSnapshot == nil {
		return ProbeFreshness(ctx, previous.Project.Root, registry, options)
	}
	if registry == nil {
		return FreshnessProbe{}, fmt.Errorf("freshness parser registry is required")
	}
	if err := ctx.Err(); err != nil {
		return FreshnessProbe{}, err
	}
	root, err := filepath.EvalSymlinks(previous.Project.Root)
	if err != nil {
		return FreshnessProbe{}, fmt.Errorf("resolve project root: %w", err)
	}
	if root != previous.Project.Root {
		return ProbeFreshness(ctx, root, registry, options)
	}
	runner := previous.Project.gitSnapshot.runner
	if runner == nil {
		runner = execGitRunner{}
	}
	identity, identityCommands, err := inspectGitIdentity(ctx, root, runner)
	if err != nil {
		return FreshnessProbe{}, err
	}
	snapshot, err := refreshGitSnapshot(ctx, previous.Project.gitSnapshot, runner)
	if err != nil {
		return FreshnessProbe{}, err
	}
	snapshot.Identity = identity
	snapshot.Commands += identityCommands
	goModule := previous.Project.GoModule
	if previous.Project.gitSnapshot == nil || previous.Project.gitSnapshot.Head != snapshot.Head ||
		previous.Project.gitSnapshot.Identity != snapshot.Identity || freshnessContainsPath(snapshot.Changed, "go.mod") {
		goModule = readGoModule(root)
	}
	project := projectFromSnapshot(root, filepath.Base(root), goModule, *snapshot, true)
	reuseWorkspaceKeys := previous.Project.gitSnapshot != nil &&
		previous.Project.gitSnapshot.Head == snapshot.Head && previous.Project.gitSnapshot.Identity == snapshot.Identity &&
		len(freshnessRelevantPaths(previous.Project.gitSnapshot.Changed, registry, projectconfig.IndexScope{})) == 0 &&
		len(freshnessRelevantPaths(snapshot.Changed, registry, projectconfig.IndexScope{})) == 0
	return probeProjectFreshness(ctx, project, registry, options, &previous, reuseWorkspaceKeys)
}

func freshnessContainsPath(paths []string, target string) bool {
	for _, path := range paths {
		if filepath.ToSlash(path) == target {
			return true
		}
	}
	return false
}

func probeProjectFreshness(ctx context.Context, project Project, registry *parserapi.Registry, options FreshnessOptions,
	previous *FreshnessProbe, reuseWorkspaceKeys bool) (FreshnessProbe, error) {
	if registry == nil {
		return FreshnessProbe{}, fmt.Errorf("freshness parser registry is required")
	}
	maximum := options.MaxFileSize
	if maximum <= 0 {
		maximum = defaultFreshnessMax
	}
	configuration, err := projectconfig.Load(project.Root)
	if err != nil {
		return FreshnessProbe{}, err
	}
	hiddenSemanticDigest, hiddenGitCommands, err := freshnessHiddenSemanticDigest(ctx, project, registry, maximum, configuration.Index)
	if err != nil {
		return FreshnessProbe{}, err
	}
	if err := ctx.Err(); err != nil {
		return FreshnessProbe{}, err
	}
	semanticInput := parserapi.Input{
		Root: project.Root, Repository: project.Name, RepoID: project.ID, GoModule: project.GoModule,
	}
	evidence, cacheable, err := registry.WorkspaceSemanticEvidenceKeys(ctx, semanticInput)
	if err != nil {
		return FreshnessProbe{}, err
	}
	var keys map[string]string
	membershipGitCommands := 0
	membershipFallback := ""
	if reuseWorkspaceKeys && cacheable && previous != nil && hiddenSemanticDigest == previous.hiddenSemanticDigest &&
		equalFreshnessMap(evidence, previous.workspaceSemanticEvidence) {
		keys = cloneFreshnessMap(previous.workspaceSemanticKeys)
	} else {
		discovered, discoverErr := discoverFilesWithCatalog(ctx, project, registry, nil, false, configuration.Index)
		if discoverErr != nil {
			return FreshnessProbe{}, fmt.Errorf("discover workspace semantic membership: %w", discoverErr)
		}
		semanticInput.SourcePaths = discovered.paths
		membershipGitCommands = discovered.gitCommands
		if len(discovered.diagnostics) > 0 {
			membershipFallback = discovered.diagnostics[0].Message
		}
		keys, err = registry.WorkspaceSemanticKeys(ctx, semanticInput)
		if err != nil {
			return FreshnessProbe{}, err
		}
	}
	if !project.GitManaged || project.gitSnapshot == nil {
		return FreshnessProbe{
			Project: project, Fallback: "non-Git project requires a conservative full refresh",
			workspaceSemanticKeys: cloneFreshnessMap(keys), workspaceSemanticEvidence: cloneFreshnessMap(evidence),
			hiddenSemanticDigest: hiddenSemanticDigest,
		}, nil
	}
	if membershipFallback != "" {
		return FreshnessProbe{
			Project: project, Fallback: membershipFallback, GitCommands: project.gitSnapshot.Commands + hiddenGitCommands + membershipGitCommands,
			workspaceSemanticKeys: cloneFreshnessMap(keys), workspaceSemanticEvidence: cloneFreshnessMap(evidence),
			hiddenSemanticDigest: hiddenSemanticDigest,
		}, nil
	}

	snapshot := project.gitSnapshot
	encoder := newFreshnessEncoder()
	encoder.addString("token-version", FreshnessTokenVersion)
	encoder.addString("semantic-index-version", SemanticIndexVersion)
	encoder.addString("identity", snapshot.Identity)
	encoder.addString("root", project.Root)
	encoder.addString("project-id", project.ID)
	encoder.addString("branch", snapshot.Branch)
	encoder.addString("detached-branch", snapshot.DetachedBranch)
	encoder.addString("head", snapshot.Head)
	encoder.addString("index-path", project.IndexPath)
	encoder.addBytes("hidden-semantic-digest", hiddenSemanticDigest[:])
	encoder.addStrings("changed", freshnessRelevantPaths(snapshot.Changed, registry, configuration.Index))
	encoder.addStrings("dirty", freshnessRelevantPaths(snapshot.Dirty, registry, configuration.Index))
	encoder.addStrings("untracked", freshnessRelevantPaths(snapshot.Untracked, registry, configuration.Index))
	for _, record := range snapshot.statusRecords {
		if freshnessRecordRelevant(record, registry, configuration.Index) {
			encoder.addString("status-record", record.encoded)
		}
	}

	configPath := filepath.Join(project.Root, projectconfig.FileName)
	if err := addFreshnessConfig(ctx, encoder, configPath); err != nil {
		return FreshnessProbe{}, err
	}
	languages := make([]string, 0, len(keys))
	for language := range keys {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	for _, language := range languages {
		encoder.addString("semantic-language", language)
		encoder.addString("semantic-key", keys[language])
	}

	paths := append(append(append([]string(nil), snapshot.Changed...), snapshot.Dirty...), snapshot.Untracked...)
	sort.Strings(paths)
	last := ""
	for _, path := range paths {
		path = filepath.ToSlash(path)
		if path == last {
			continue
		}
		last = path
		semanticInput := registry.IsSemanticDependency(path)
		if !configuration.Index.Allows(path) || (PathIgnored(path) && !semanticInput) || path == projectconfig.FileName {
			continue
		}
		if _, ok := registry.For(path); !ok && !semanticInput {
			continue
		}
		encoder.addString("status-path", path)
		absolute, err := safeFreshnessPath(project.Root, path)
		if err != nil {
			return FreshnessProbe{}, err
		}
		if err := addFreshnessFile(ctx, encoder, "source:"+path, absolute, maximum); err != nil {
			return FreshnessProbe{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return FreshnessProbe{}, err
	}
	return FreshnessProbe{
		Project: project, Token: FreshnessToken{version: FreshnessTokenVersion, digest: encoder.sum()},
		Supported: true, GitCommands: snapshot.Commands + hiddenGitCommands + membershipGitCommands,
		workspaceSemanticKeys: cloneFreshnessMap(keys), workspaceSemanticEvidence: cloneFreshnessMap(evidence),
		hiddenSemanticDigest: hiddenSemanticDigest,
	}, nil
}

func freshnessHiddenSemanticDigest(ctx context.Context, project Project, registry *parserapi.Registry, maximum int64,
	scope projectconfig.IndexScope,
) ([sha256.Size]byte, int, error) {
	encoder := newFreshnessEncoder()
	encoder.addString("hidden-semantic-version", "hidden-semantic-v1")
	if !project.GitManaged || project.gitSnapshot == nil {
		return encoder.sum(), 0, nil
	}
	runner := gitCommandRunner(execGitRunner{})
	if project.gitSnapshot.runner != nil {
		runner = project.gitSnapshot.runner
	}
	visibleRaw, err := runner.Run(ctx, project.Root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return [sha256.Size]byte{}, 1, fmt.Errorf("enumerate Git-visible semantic inputs: %w", err)
	}
	visible := make(map[string]bool)
	for _, raw := range bytes.Split(visibleRaw, []byte{0}) {
		if len(raw) != 0 {
			visible[filepath.ToSlash(string(raw))] = true
		}
	}
	err = filepath.WalkDir(project.Root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := filepath.Rel(project.Root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative != "." && repositorypath.DirectoryIgnored(entry.Name()) && entry.Name() != "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if relative == projectconfig.FileName ||
			strings.EqualFold(filepath.Base(relative), "project.godot") && !visible[relative] {
			return nil
		}
		if !scope.Allows(relative) {
			return nil
		}
		if repositorypath.Ignored(relative) && !vendoredSemanticPath(relative) {
			return nil
		}
		if visible[relative] || !registry.IsSemanticDependency(relative) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect hidden semantic input %s: %w", relative, err)
		}
		if info.Mode().IsRegular() && maximum >= 0 && info.Size() > maximum {
			return fmt.Errorf("hidden semantic input %s exceeds freshness bound %d", relative, maximum)
		}
		encoder.addString("hidden-semantic-path", relative)
		return addFreshnessFile(ctx, encoder, "hidden-semantic:"+relative, path, maximum)
	})
	if err != nil {
		return [sha256.Size]byte{}, 1, err
	}
	return encoder.sum(), 1, nil
}

func vendoredSemanticPath(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == "vendor" {
			return true
		}
	}
	return false
}

func cloneFreshnessMap(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func equalFreshnessMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func addFreshnessConfig(ctx context.Context, encoder *freshnessEncoder, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		encoder.addString("config:state", "missing")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read effective project configuration: %w", err)
	}
	digest := sha256.Sum256(content)
	encoder.addString("config:state", "effective")
	encoder.addBytes("config:digest", digest[:])
	return nil
}

func freshnessRelevantPaths(paths []string, registry *parserapi.Registry, scope projectconfig.IndexScope) []string {
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.ToSlash(path)
		if !scope.Allows(path) {
			continue
		}
		semanticInput := registry.IsSemanticDependency(path)
		if PathIgnored(path) && !semanticInput {
			continue
		}
		if path == projectconfig.FileName {
			result = append(result, path)
			continue
		}
		if semanticInput {
			result = append(result, path)
			continue
		}
		if _, ok := registry.For(path); ok {
			result = append(result, path)
		}
	}
	return result
}

func freshnessRecordRelevant(record gitStatusRecord, registry *parserapi.Registry, scope projectconfig.IndexScope) bool {
	for _, path := range record.paths {
		if len(freshnessRelevantPaths([]string{path}, registry, scope)) != 0 {
			return true
		}
	}
	return false
}

type freshnessEncoder struct{ hash hash.Hash }

func newFreshnessEncoder() *freshnessEncoder { return &freshnessEncoder{hash: sha256.New()} }

func (e *freshnessEncoder) addString(label, value string) {
	e.addBytes(label, []byte(value))
}

func (e *freshnessEncoder) addStrings(label string, values []string) {
	for _, value := range values {
		e.addString(label, value)
	}
}

func (e *freshnessEncoder) addBytes(label string, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(label)))
	_, _ = e.hash.Write(size[:])
	_, _ = e.hash.Write([]byte(label))
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = e.hash.Write(size[:])
	_, _ = e.hash.Write(value)
}

func (e *freshnessEncoder) sum() [sha256.Size]byte {
	var result [sha256.Size]byte
	copy(result[:], e.hash.Sum(nil))
	return result
}

func safeFreshnessPath(root, relative string) (string, error) {
	absolute, err := repositorypath.ResolvePath(root, relative)
	if err != nil {
		return "", fmt.Errorf("resolve freshness path %q: %w", relative, err)
	}
	return absolute, nil
}

func addFreshnessFile(ctx context.Context, encoder *freshnessEncoder, label, path string, maximum int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		encoder.addString(label+":state", "missing")
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect freshness input %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, readErr := os.Readlink(path)
		if readErr != nil {
			return fmt.Errorf("read freshness symlink %s: %w", path, readErr)
		}
		encoder.addString(label+":state", "symlink")
		encoder.addString(label+":target", target)
		return nil
	}
	if !info.Mode().IsRegular() {
		encoder.addString(label+":state", "non-regular:"+info.Mode().String())
		return nil
	}
	if maximum >= 0 && info.Size() > maximum {
		encoder.addString(label+":state", fmt.Sprintf("oversize:%d", info.Size()))
		return nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read freshness input %s: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	encoder.addString(label+":state", "regular")
	encoder.addBytes(label+":digest", digest[:])
	return nil
}
