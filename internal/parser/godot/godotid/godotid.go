// Package godotid is the single source of truth for Godot resource identity
// and for the project-level declarations that several Godot producers share.
//
// Every Godot producer - text scenes and resources, UID sidecars, project
// configuration, and GDScript - must derive resource identity here so one
// resource has exactly one canonical qualified name no matter which evidence
// (a repository-relative path, a res:// path, or a uid:// alias) named it.
// Canonical identity is the repository-relative path without its extension,
// because sidecars and remaps can change a UID while the path stays stable;
// UIDs are aliases and evidence, never the identity itself.
package godotid

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
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

// Canonical returns the canonical qualified name for a Godot resource
// reference, or "" when the reference carries no path evidence (a bare uid://
// alias, or an empty value). Leading autoload markers and res:// or user://
// prefixes are stripped, and the extension is dropped so one resource has the
// same identity in every producer.
func Canonical(reference string) string {
	reference = strings.TrimSpace(reference)
	reference = strings.TrimPrefix(reference, "*")
	if reference == "" || IsUID(reference) {
		return ""
	}
	reference = strings.TrimPrefix(reference, "res://")
	reference = strings.TrimPrefix(reference, "user://")
	reference = filepath.ToSlash(reference)
	extension := strings.ToLower(filepath.Ext(reference))
	return strings.TrimSuffix(reference, extension)
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

// AutoloadQualifiedName returns the project-scoped identity of an autoload.
func AutoloadQualifiedName(name string) string { return AutoloadPrefix + strings.TrimSpace(name) }

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
	// Digest fingerprints the autoload vocabulary so an incremental index can
	// invalidate scripts when it changes.
	Digest string
}

// Autoload returns the unambiguous declaration for a name.
func (p Project) Autoload(name string) (Autoload, bool) {
	declaration, ok := p.Autoloads[strings.TrimSpace(name)]
	return declaration, ok
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
	_, _ = digest.Write([]byte("godotid-autoload-v1"))
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
			declaration := NewAutoload(name, literal.Value, line)
			if declaration.Target == "" && declaration.UID == "" {
				malformed[name] = true
				continue
			}
			project.Autoloads[name] = declaration
		}
	}
	for _, name := range sortedKeys(project.Autoloads) {
		declaration := project.Autoloads[name]
		_, _ = digest.Write([]byte(name + "\x00" + declaration.Reference + "\x00"))
		if declaration.Enabled {
			_, _ = digest.Write([]byte("*"))
		}
		_, _ = digest.Write([]byte{0})
	}
	project.Conflicts = setKeys(conflicts)
	project.Malformed = setKeys(malformed)
	for _, name := range project.Conflicts {
		_, _ = digest.Write([]byte("conflict:" + name + "\x00"))
	}
	project.Digest = hex.EncodeToString(digest.Sum(nil))
	return project
}

// NewAutoload interprets one raw [autoload] value.
func NewAutoload(name, value string, line int) Autoload {
	trimmed := strings.TrimSpace(value)
	enabled := strings.HasPrefix(trimmed, "*")
	reference := strings.TrimPrefix(trimmed, "*")
	return Autoload{
		Name: strings.TrimSpace(name), Reference: reference, Target: Canonical(reference),
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
