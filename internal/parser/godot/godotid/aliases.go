package godotid

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Alias scanning reads only the header of each candidate file. A UID always
// appears in the first section of a text resource and in the remap block of an
// import file, so a bounded read is enough and no whole project is loaded into
// memory.
const (
	headerReadLimit = 8 << 10
	uidReadLimit    = 1 << 10
)

// skippedDirectories are never scanned for UID declarations: they hold Godot's
// own generated cache, Grafo's indexes, version control data, or vendored
// dependencies, none of which own project resources.
var skippedDirectories = map[string]bool{
	".git": true, ".godot": true, ".grafo": true, ".hg": true, ".svn": true, "node_modules": true,
}

var uidPattern = regexp.MustCompile(`uid="(uid://[^"]*)"`)

// Aliases is the repository-wide UID alias table: which resource declares each
// uid:// alias. It exists so a reference carrying both a UID and a path can be
// checked for agreement, because a path that contradicts its UID would otherwise
// produce a confident edge to the wrong resource.
//
// A resource declares its own UID: text scenes and resources in their header,
// scripts and imported assets in a .uid or .import sidecar. Nothing else is
// authoritative - an ext_resource pairing is evidence about a reference, not a
// declaration - so only declarations are collected here.
type Aliases struct {
	// Digest fingerprints the table plus the set of project.godot locations, so
	// an incremental index reparses Godot files when either changes.
	Digest string
	// Projects lists every repository-relative project.godot found, sorted.
	Projects []string
	byUID    map[string][]string
}

// Declared returns the canonical identities that declare a UID. The second
// result reports whether the UID is declared at all: an undeclared UID cannot
// contradict a path, while a UID declared by more than one resource is
// ambiguous and must resolve nothing.
func (a *Aliases) Declared(uid string) ([]string, bool) {
	if a == nil || uid == "" {
		return nil, false
	}
	identities, ok := a.byUID[uid]
	return identities, ok
}

// Agrees reports whether path evidence is consistent with UID evidence: the
// resource that declares the UID must be the one the path names. Absence is not
// contradiction, so a UID no resource declares leaves the path standing and an
// absent target stays an explicit external node. A UID declared by several
// resources never agrees, because picking one of them would be a guess.
//
// The check runs in this direction only. The reverse question - does the
// resource the path names declare some other UID? - cannot be answered from
// canonical identity, because identity drops the extension and a scene and its
// script therefore share one identity while declaring two different UIDs of
// their own. Asking it rejected 95 correct references in a real project and
// zero incorrect ones.
func (a *Aliases) Agrees(uid, identity string) bool {
	declared, ok := a.Declared(uid)
	if !ok {
		return true
	}
	return len(declared) == 1 && declared[0] == identity
}

// Describe renders the declaring resources of a UID for a diagnostic.
func (a *Aliases) Describe(uid string) string {
	declared, _ := a.Declared(uid)
	if len(declared) == 0 {
		return "nothing"
	}
	return strings.Join(declared, ", ")
}

type aliasCacheEntry struct{ aliases *Aliases }

var (
	aliasMu    sync.Mutex
	aliasCache = map[string]aliasCacheEntry{}
)

// LoadAliases scans a repository for UID declarations and project locations,
// replacing any cached table. The indexer calls this once per run through the
// workspace semantic key, which is what keeps the cached table fresh.
func LoadAliases(root string) (*Aliases, error) {
	if root == "" {
		return &Aliases{byUID: map[string][]string{}}, nil
	}
	aliases, err := scanAliases(root)
	if err != nil {
		return nil, err
	}
	aliasMu.Lock()
	aliasCache[root] = aliasCacheEntry{aliases: aliases}
	aliasMu.Unlock()
	return aliases, nil
}

// AliasesFor returns the cached alias table for a repository, scanning once if
// no run has loaded it yet.
func AliasesFor(root string) (*Aliases, error) {
	if root == "" {
		return &Aliases{byUID: map[string][]string{}}, nil
	}
	aliasMu.Lock()
	entry, ok := aliasCache[root]
	aliasMu.Unlock()
	if ok {
		return entry.aliases, nil
	}
	return LoadAliases(root)
}

func scanAliases(root string) (*Aliases, error) {
	aliases := &Aliases{byUID: map[string][]string{}}
	declarations := map[string]map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != root && skippedDirectories[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if filepath.Base(relative) == ProjectFileName {
			aliases.Projects = append(aliases.Projects, relative)
			return nil
		}
		uid, owner := scanDeclaration(path, relative)
		if uid == "" || owner == "" {
			return nil
		}
		if declarations[uid] == nil {
			declarations[uid] = map[string]bool{}
		}
		declarations[uid][owner] = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan Godot UID declarations: %w", err)
	}
	for uid, declaring := range declarations {
		aliases.byUID[uid] = sortedSet(declaring)
	}
	sort.Strings(aliases.Projects)
	aliases.Digest = aliasDigest(aliases)
	return aliases, nil
}

// scanDeclaration returns the UID a file declares for itself and the canonical
// identity that owns it.
func scanDeclaration(path, relative string) (string, string) {
	switch strings.ToLower(filepath.Ext(relative)) {
	case ".uid":
		content, err := readPrefix(path, uidReadLimit)
		if err != nil {
			return "", ""
		}
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "uid://") {
				return line, Identity(strings.TrimSuffix(relative, filepath.Ext(relative)))
			}
		}
		return "", ""
	case ".import":
		content, err := readPrefix(path, headerReadLimit)
		if err != nil {
			return "", ""
		}
		if match := uidPattern.FindSubmatch(content); match != nil {
			return string(match[1]), Identity(strings.TrimSuffix(relative, filepath.Ext(relative)))
		}
		return "", ""
	case ".tscn", ".tres", ".escn":
		content, err := readPrefix(path, headerReadLimit)
		if err != nil {
			return "", ""
		}
		// Only the document header declares this resource's own UID; an
		// ext_resource line names some other resource's alias.
		for _, line := range strings.Split(string(content), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "[gd_scene") && !strings.HasPrefix(trimmed, "[gd_resource") {
				continue
			}
			if match := uidPattern.FindStringSubmatch(trimmed); match != nil {
				return match[1], Identity(relative)
			}
			return "", ""
		}
		return "", ""
	default:
		return "", ""
	}
}

func sortedSet(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func readPrefix(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var buffer bytes.Buffer
	if _, err := io.Copy(&buffer, io.LimitReader(file, limit)); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func aliasDigest(aliases *Aliases) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("godotid-aliases-v1"))
	for _, project := range aliases.Projects {
		_, _ = digest.Write([]byte("project\x00" + project + "\x00"))
	}
	uids := make([]string, 0, len(aliases.byUID))
	for uid := range aliases.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		_, _ = digest.Write([]byte("uid\x00" + uid + "\x00" + strings.Join(aliases.byUID[uid], "\x00") + "\x00"))
	}
	return hex.EncodeToString(digest.Sum(nil))
}
