// Package godotid is the single source of truth for Godot resource identity
// and for the project-level declarations that several Godot producers share.
//
// Every Godot producer - text scenes and resources, UID sidecars, project
// configuration, shaders, and GDScript - must derive resource identity here so
// one resource has exactly one canonical qualified name no matter which evidence
// (a repository-relative path, a res:// path, or a uid:// alias) named it.
// Canonical identity is the repository-relative path without its extension,
// because sidecars and remaps can change a UID while the path stays stable;
// UIDs are aliases and evidence, never the identity itself.
//
// Identity and reference resolution are separate on purpose. Identity takes a
// repository-relative path; Resolve takes a Godot reference plus the project
// that owns it, because res:// is relative to a project.godot directory that can
// sit anywhere in a repository. Canonicalizing a reference without that
// directory silently targets identities no file owns, so there is deliberately
// no single-argument entry point that accepts a res:// reference.
package godotid

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/cafecito-games/gdparser/configfile"
	configast "github.com/cafecito-games/gdparser/configfile/ast"
	"github.com/cafecito-games/grafo/internal/graph"
)

// ProjectFileName is the tracked Godot project configuration file. Its
// directory is the root of one Godot project, which may sit anywhere inside an
// indexed repository.
const ProjectFileName = "project.godot"

// AutoloadSection is the project.godot section that declares autoloads.
const AutoloadSection = "autoload"

// AutoloadPrefix scopes autoload identities to the Godot project vocabulary so
// an autoload named Game never collides with a class or module named Game.
const AutoloadPrefix = "godot:autoload:"

// Class is the kind of resource a Godot path names. It is derived from the
// file extension only: no file is read and no import metadata is consulted, so
// the same path always classifies the same way.
type Class string

const (
	// ClassScene is a packed scene, text (.tscn/.escn) or binary (.scn).
	ClassScene Class = "scene"
	// ClassScript is a script attached to a node or resource.
	ClassScript Class = "script"
	// ClassShader is a shader or shader include.
	ClassShader Class = "shader"
	// ClassResource is any other Godot resource, including saved resources
	// (.tres/.res) and imported assets such as textures and audio.
	ClassResource Class = "resource"
)

// Classify reports which Godot resource class a path names.
func Classify(path string) Class {
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(path))) {
	case ".tscn", ".escn", ".scn":
		return ClassScene
	case ".gd", ".cs":
		return ClassScript
	case ".gdshader", ".gdshaderinc":
		return ClassShader
	default:
		return ClassResource
	}
}

// NodeKind maps a resource class onto the graph vocabulary. Scripts and
// shaders keep the generic module kind because their own parsers own those
// nodes; scenes and resources use the Godot kinds.
func NodeKind(class Class) graph.NodeKind {
	switch class {
	case ClassScene:
		return graph.KindGodotScene
	case ClassScript, ClassShader:
		return graph.KindModule
	default:
		return graph.KindGodotResource
	}
}

// TargetKind classifies a path and returns the graph kind a fact should target.
func TargetKind(path string) graph.NodeKind { return NodeKind(Classify(path)) }

// Identity returns the canonical identity of a repository-relative Godot
// resource path: the path without its extension. Callers that hold a res:// or
// user:// reference must use Resolve instead, because such a reference is
// relative to its own Godot project rather than to the repository.
func Identity(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = strings.TrimPrefix(filepath.ToSlash(path), "./")
	extension := filepath.Ext(path)
	// A name that is only an extension (".tscn") has no identity to trim; keep
	// it whole rather than collapsing every such file onto the empty name.
	if extension != "" && extension != pathpkg.Base(path) {
		path = path[:len(path)-len(extension)]
	}
	return path
}

// Resolve returns the canonical repository-relative identity of a Godot
// resource reference as seen from a project rooted at projectDir, which is the
// repository-relative directory holding project.godot ("" when the project root
// and the repository root are the same, or when no project file was found).
//
// A res:// or user:// reference is project-relative: Godot resolves res://a.tscn
// against the project directory, so a project in a monorepo subdirectory must
// resolve it under that subdirectory or every relationship would target an
// identity no file owns. A reference with no scheme is already
// repository-relative and is left where it is. A bare uid:// alias carries no
// path evidence and resolves to "".
//
// A scheme-qualified reference that traverses out of its own project resolves to
// "" rather than to whatever it lands on. res:// names a location inside one
// project by definition, so escaping it is not a valid reference, and in a
// monorepo the thing it lands on usually belongs to a different Godot project.
// Callers distinguish that case from "no path evidence" with EscapesProject.
func Resolve(projectDir, reference string) string {
	identity, _ := resolve(projectDir, reference)
	return identity
}

// EscapesProject reports whether a reference carries path evidence that leaves
// the project (or the repository, for a project rooted at it). Such a reference
// resolves to nothing, and a caller that can diagnose should say so rather than
// stay silent.
func EscapesProject(projectDir, reference string) bool {
	_, escaped := resolve(projectDir, reference)
	return escaped
}

func resolve(projectDir, reference string) (string, bool) {
	reference = strings.TrimPrefix(strings.TrimSpace(reference), "*")
	reference = strings.TrimSpace(reference)
	if reference == "" || IsUID(reference) {
		return "", false
	}
	scoped := false
	for _, prefix := range []string{"res://", "user://"} {
		if strings.HasPrefix(reference, prefix) {
			reference, scoped = strings.TrimPrefix(reference, prefix), true
			break
		}
	}
	reference = strings.TrimLeft(filepath.ToSlash(reference), "/")
	if reference == "" {
		return "", false
	}
	projectDir = strings.Trim(strings.TrimSpace(filepath.ToSlash(projectDir)), "/")
	if scoped && projectDir != "" {
		// Resolve inside the project first, then confirm the result stayed there.
		cleaned := pathpkg.Clean(projectDir + "/" + reference)
		if cleaned != projectDir && !strings.HasPrefix(cleaned, projectDir+"/") {
			return "", true
		}
		return Identity(cleaned), false
	}
	cleaned := pathpkg.Clean(reference)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", scoped
	}
	return Identity(cleaned), false
}

// IsUID reports whether a reference is a uid:// alias rather than a path.
func IsUID(reference string) bool {
	return strings.HasPrefix(strings.TrimPrefix(strings.TrimSpace(reference), "*"), "uid://")
}

// UID normalizes a uid:// alias, returning "" when the reference is not one.
func UID(reference string) string {
	reference = strings.TrimPrefix(strings.TrimSpace(reference), "*")
	if !strings.HasPrefix(reference, "uid://") {
		return ""
	}
	return reference
}

// AutoloadQualifiedName returns the identity of an autoload, scoped to the
// repository-relative project.godot that declares it. Ownership is per project,
// so two Godot projects in one repository that both declare Game are two
// distinct singletons and must not collide on one node.
//
// Any further project-scoped Godot vocabulary - input actions and node groups,
// for instance - must be scoped the same way and for the same reason.
func AutoloadQualifiedName(projectPath, name string) string {
	return projectScoped(AutoloadPrefix, projectPath, name)
}

// projectScoped builds a project-scoped identity from a vocabulary prefix, the
// declaring project.godot path, and a declared name.
func projectScoped(prefix, projectPath, name string) string {
	return prefix + strings.TrimSpace(projectPath) + ":" + strings.TrimSpace(name)
}

// SceneNodeQualifiedName returns the identity of one node inside a scene. The
// scene is named canonically and the node path is the scene-root-relative path
// the scene file declares.
func SceneNodeQualifiedName(scene, nodePath string) string { return scene + ":" + nodePath }

// SubResourceQualifiedName returns the identity of a resource embedded in a
// scene or resource file under its declared section id.
func SubResourceQualifiedName(owner, id string) string { return owner + "#" + id }

// Autoload is one declaration from the project.godot [autoload] section.
type Autoload struct {
	// Name is the global identifier scripts use.
	Name string
	// Reference is the declared value with its enabled marker removed, kept
	// verbatim as evidence (for example "res://core/log.gd").
	Reference string
	// Target is the canonical identity of Reference, or "" when the value
	// carried no path evidence.
	Target string
	// UID is the uid:// alias when the declaration used one instead of a path.
	UID string
	// Enabled reports the leading "*" marker, which Godot writes for an
	// autoload exposed as a global singleton.
	Enabled bool
	// Line is the declaration's line in project.godot.
	Line int
}

// Project is the resolved autoload vocabulary of one Godot project.
type Project struct {
	// Path is the repository-relative path of the project.godot that declared
	// these autoloads, or "" when no project file was found.
	Path string
	// Autoloads holds only unambiguous declarations, keyed by name. A name
	// declared more than once is deliberately absent: conflicting
	// declarations must resolve nothing rather than pick one.
	Autoloads map[string]Autoload
	// Conflicts lists names declared more than once, sorted.
	Conflicts []string
	// Malformed lists names whose value is not a resource string, sorted.
	Malformed []string
	// Lines records the first declaration line of every declared name,
	// including conflicting and malformed ones, so diagnostics keep their
	// source location.
	Lines map[string]int
	// Digest fingerprints the project vocabulary that dependent files resolve
	// against, so an incremental index reparses them when it changes. Every
	// declaration kind this type grows must be folded into it, or an edit to
	// that section will not invalidate the files whose extraction it changes.
	Digest string
}

// Autoload returns the unambiguous declaration for a name, enabled or not.
func (p Project) Autoload(name string) (Autoload, bool) {
	declaration, ok := p.Autoloads[strings.TrimSpace(name)]
	return declaration, ok
}

// Singleton returns the declaration for a name only when Godot exposes it as a
// global singleton. An autoload declared without the leading "*" marker is not
// available as a global identifier, so a script use of that name must resolve to
// nothing rather than to a node the engine never registers.
func (p Project) Singleton(name string) (Autoload, bool) {
	declaration, ok := p.Autoload(name)
	if !ok || !declaration.Enabled {
		return Autoload{}, false
	}
	return declaration, true
}

// Dir returns the repository-relative directory of this Godot project, or ""
// when the project root is the repository root or no project file was found.
func (p Project) Dir() string { return DirOf(p.Path) }

// Resolve canonicalizes a Godot resource reference inside this project.
func (p Project) Resolve(reference string) string { return Resolve(p.Dir(), reference) }

// AutoloadQualifiedName returns the identity of one of this project's autoloads.
func (p Project) AutoloadQualifiedName(name string) string {
	return AutoloadQualifiedName(p.Path, name)
}

// SemanticKey fingerprints everything about this project that can change a
// dependent file's extraction: which project owns the file, and the autoload
// vocabulary that file's uses resolve against.
func (p Project) SemanticKey() string { return p.Path + "\x00" + p.Digest }

// DirOf returns the repository-relative directory holding a project.godot, or
// "" when the file sits at the repository root or the path is empty.
func DirOf(projectPath string) string {
	projectPath = strings.TrimSpace(projectPath)
	if projectPath == "" {
		return ""
	}
	directory := pathpkg.Dir(filepath.ToSlash(projectPath))
	if directory == "." || directory == "/" {
		return ""
	}
	return directory
}

type cacheEntry struct {
	modified int64
	size     int64
	project  Project
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

// LoadProject resolves the Godot project that owns one repository-relative
// file and returns its autoload vocabulary. It walks from the file's directory
// up to the repository root looking for project.godot, so Godot projects
// nested in a monorepo resolve against their own configuration. A repository
// without a project file yields an empty Project and no error.
func LoadProject(root, path string) (Project, error) {
	if root == "" {
		return Project{}, nil
	}
	directory := filepath.Dir(filepath.FromSlash(strings.TrimPrefix(path, "./")))
	for {
		candidate := filepath.Join(root, directory, ProjectFileName)
		info, err := os.Stat(candidate)
		switch {
		case err == nil && !info.IsDir():
			relative := filepath.ToSlash(filepath.Join(directory, ProjectFileName))
			relative = strings.TrimPrefix(relative, "./")
			return loadFile(candidate, relative, info)
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return Project{}, err
		}
		if directory == "." || directory == string(filepath.Separator) {
			return Project{}, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return Project{}, nil
		}
		directory = parent
	}
}

func loadFile(absolute, relative string, info os.FileInfo) (Project, error) {
	key := absolute
	modified, size := info.ModTime().UnixNano(), info.Size()
	cacheMu.Lock()
	entry, ok := cache[key]
	cacheMu.Unlock()
	if ok && entry.modified == modified && entry.size == size {
		return entry.project, nil
	}
	content, err := os.ReadFile(absolute)
	if err != nil {
		return Project{}, err
	}
	project := ParseProject(relative, content)
	cacheMu.Lock()
	cache[key] = cacheEntry{modified: modified, size: size, project: project}
	cacheMu.Unlock()
	return project, nil
}

// ParseProject extracts the autoload vocabulary from project.godot content. A
// parse failure yields an empty vocabulary rather than an error: the config
// extractor owns reporting malformed project files, and a script must not
// resolve autoloads from a file that did not parse.
func ParseProject(relative string, content []byte) Project {
	file, err := configfile.ParseFile(relative, content)
	if err != nil {
		return ProjectFromFile(relative, nil)
	}
	return ProjectFromFile(relative, file)
}

// ProjectFromFile extracts the autoload vocabulary from an already parsed
// project.godot. The configuration extractor and the GDScript parser both go
// through here so one set of rules decides which declarations are exact.
func ProjectFromFile(relative string, file *configast.File) Project {
	project := Project{Path: relative, Autoloads: map[string]Autoload{}, Lines: map[string]int{}}
	digest := sha256.New()
	_, _ = digest.Write([]byte("godotid-project-v2"))
	if file == nil {
		project.Digest = hex.EncodeToString(digest.Sum(nil))
		return project
	}
	seen := map[string]bool{}
	conflicts, malformed := map[string]bool{}, map[string]bool{}
	for _, section := range file.Sections {
		if !strings.EqualFold(section.Name, AutoloadSection) {
			continue
		}
		for _, statement := range section.Statements {
			assignment, ok := statement.(*configast.Assignment)
			if !ok {
				continue
			}
			name := strings.TrimSpace(assignment.Key)
			if name == "" {
				continue
			}
			line := assignment.Span().Start.Line
			if _, recorded := project.Lines[name]; !recorded {
				project.Lines[name] = line
			}
			if seen[name] {
				conflicts[name] = true
				delete(project.Autoloads, name)
				continue
			}
			seen[name] = true
			literal, ok := assignment.Value.(*configast.StringLiteral)
			if !ok {
				malformed[name] = true
				continue
			}
			declaration := NewAutoload(project.Dir(), name, literal.Value, line)
			if declaration.Target == "" && declaration.UID == "" {
				malformed[name] = true
				continue
			}
			project.Autoloads[name] = declaration
		}
	}
	// Extend this loop, not just the Project fields, when adding a declaration
	// kind: the digest is what makes an incremental index equal a clean rebuild.
	//
	// Every field of a declaration is folded in, not only the ones an extractor
	// reads today, because a later extractor that starts reading one must not
	// depend on someone remembering to extend this. Enabled is the live example:
	// script-side resolution reads it, and a digest over the marker-stripped
	// reference alone would let a toggled singleton leave a stale edge behind.
	for _, name := range sortedKeys(project.Autoloads) {
		declaration := project.Autoloads[name]
		_, _ = digest.Write([]byte("autoload\x00" + name + "\x00" + declaration.Reference + "\x00" +
			declaration.Target + "\x00" + declaration.UID + "\x00" +
			strconv.FormatBool(declaration.Enabled) + "\x00" +
			strconv.Itoa(declaration.Line) + "\x00"))
	}
	project.Conflicts = setKeys(conflicts)
	project.Malformed = setKeys(malformed)
	for _, name := range project.Conflicts {
		_, _ = digest.Write([]byte("conflict\x00" + name + "\x00"))
	}
	for _, name := range project.Malformed {
		_, _ = digest.Write([]byte("malformed\x00" + name + "\x00"))
	}
	project.Digest = hex.EncodeToString(digest.Sum(nil))
	return project
}

// NewAutoload interprets one raw [autoload] value. The target is resolved
// against the declaring project's directory, because an autoload path is a
// res:// reference like any other.
func NewAutoload(projectDir, name, value string, line int) Autoload {
	trimmed := strings.TrimSpace(value)
	enabled := strings.HasPrefix(trimmed, "*")
	reference := strings.TrimPrefix(trimmed, "*")
	return Autoload{
		Name: strings.TrimSpace(name), Reference: reference, Target: Resolve(projectDir, reference),
		UID: UID(reference), Enabled: enabled, Line: line,
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func setKeys(values map[string]bool) []string {
	if len(values) == 0 {
		return nil
	}
	return sortedKeys(values)
}
