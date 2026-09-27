package godotid

import (
	"bufio"
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

// Alias scanning reads each candidate file only as far as its own UID
// declaration can appear: the first section of a text resource, the remap block
// of an import file, the single line of a .uid sidecar. The budget bounds memory
// without turning a long file into a wrong answer - a file whose verdict is not
// reached within it is recorded as unknown rather than as undeclared, because
// undeclared evidence is treated as agreement and unknown evidence must not be.
const scanBudget = 1 << 20

// scanVerdict is what a scan could establish about one file.
type scanVerdict int

const (
	// verdictNone means the file conclusively declares no UID.
	verdictNone scanVerdict = iota
	// verdictDeclared means a UID was read.
	verdictDeclared
	// verdictUnknown means the scan could not determine whether the file
	// declares a UID: the budget ran out before the verdict, or the file could
	// not be read.
	verdictUnknown
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
	// Unknown lists files whose UID declaration could not be determined,
	// sorted. While it is non-empty the table cannot prove that a UID is
	// undeclared, so Agrees stops treating absence as agreement.
	Unknown []string
	byUID   map[string][]string
}

// Incomplete reports whether the table can prove that a UID is undeclared. A
// nil table knows nothing at all, so it is always incomplete.
func (a *Aliases) Incomplete() bool { return a == nil || len(a.Unknown) > 0 }

// UnknownAliases returns a table that knows nothing about a repository, for the
// case where scanning it failed. Every UID check against it fails closed.
func UnknownAliases(root string) *Aliases {
	label := strings.TrimSpace(root)
	if label == "" {
		label = "<repository>"
	}
	return &Aliases{byUID: map[string][]string{}, Unknown: []string{label}}
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

// Agrees reports whether path evidence is consistent with UID evidence: the UID
// must be declared exactly once, by the resource the path names.
//
// Both halves of that sentence need the table to be complete, so an incomplete
// table agrees with nothing. Absence is not contradiction - a UID no resource
// declares leaves exact path evidence standing, and an absent target stays an
// explicit external node - but only when the absence can be proven; an unscanned
// file might hold the declaration. Uniqueness needs the same proof in the
// positive direction: finding a UID once among the files that could be read is
// not finding it once, because an unscanned file might declare it too, and
// accepting it anyway would resolve an edge on unproven uniqueness.
//
// A UID declared by several resources never agrees either, because picking one of
// them would be a guess.
//
// The check runs in this direction only. The reverse question - does the
// resource the path names declare some other UID? - cannot be answered from
// canonical identity, because identity drops the extension and a scene and its
// script therefore share one identity while declaring two different UIDs of
// their own. Asking it rejected 95 correct references in a real project and
// zero incorrect ones.
func (a *Aliases) Agrees(uid, identity string) bool {
	if a.Incomplete() {
		return false
	}
	declared, ok := a.Declared(uid)
	if !ok {
		return true
	}
	return len(declared) == 1 && declared[0] == identity
}

// DisagreementReason explains, for a diagnostic, why UID and path evidence could
// not be accepted. It names the specific obstacle - the resource that declares the
// UID instead, or the file that could not be read - so the reference can be fixed
// rather than merely reported.
func (a *Aliases) DisagreementReason(uid, identity string) string {
	if a.Incomplete() {
		return fmt.Sprintf("%s could not be scanned, so no declaration of %s can be proven unique",
			a.describeUnknown(), uid)
	}
	declared, ok := a.Declared(uid)
	switch {
	case !ok:
		return fmt.Sprintf("%s is declared by nothing", uid)
	case len(declared) > 1:
		return fmt.Sprintf("%s is declared by %s", uid, strings.Join(declared, " and "))
	default:
		return fmt.Sprintf("%s is declared by %s", uid, declared[0])
	}
}

// describeUnknown names the files whose declaration status is unknown, bounded so
// one broken checkout cannot produce an unreadable diagnostic.
func (a *Aliases) describeUnknown() string {
	if a == nil || len(a.Unknown) == 0 {
		return "nothing"
	}
	const named = 3
	if len(a.Unknown) <= named {
		return strings.Join(a.Unknown, ", ")
	}
	return fmt.Sprintf("%s and %d more file(s)",
		strings.Join(a.Unknown[:named], ", "), len(a.Unknown)-named)
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
		uid, owner, verdict := scanDeclaration(path, relative)
		if verdict == verdictUnknown {
			aliases.Unknown = append(aliases.Unknown, relative)
			return nil
		}
		if verdict != verdictDeclared || uid == "" || owner == "" {
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
	sort.Strings(aliases.Unknown)
	aliases.Digest = aliasDigest(aliases)
	return aliases, nil
}

// scanDeclaration returns the UID a file declares for itself, the canonical
// identity that owns it, and what the scan could establish. A file type that
// cannot declare a UID reports verdictNone without being read.
func scanDeclaration(path, relative string) (string, string, scanVerdict) {
	extension := strings.ToLower(filepath.Ext(relative))
	sidecar := Identity(strings.TrimSuffix(relative, filepath.Ext(relative)))
	switch extension {
	case ".uid":
		uid, verdict := scanLines(path, func(line string) (string, bool) {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "uid://") {
				return trimmed, true
			}
			return "", false
		})
		return uid, sidecar, verdict
	case ".import":
		uid, verdict := scanLines(path, func(line string) (string, bool) {
			if match := uidPattern.FindStringSubmatch(line); match != nil {
				return match[1], true
			}
			return "", false
		})
		return uid, sidecar, verdict
	case ".tscn", ".tres", ".escn":
		// Only the document header declares this resource's own UID; an
		// ext_resource line names some other resource's alias. The header is
		// therefore also the point at which the verdict is settled.
		uid, verdict := scanLines(path, func(line string) (string, bool) {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "[gd_scene") && !strings.HasPrefix(trimmed, "[gd_resource") {
				return "", false
			}
			if match := uidPattern.FindStringSubmatch(trimmed); match != nil {
				return match[1], true
			}
			return "", true
		})
		return uid, Identity(relative), verdict
	default:
		return "", "", verdictNone
	}
}

// scanLines reads a file line by line until decide settles the verdict, the file
// ends, or the budget is exhausted. The budget bounds the read itself, so a file
// with one enormous line cannot exhaust memory, and a line that the budget cut
// short is never handed to decide: a truncated header could match its opening
// token while hiding the UID that follows, which would settle the verdict the
// wrong way. Reaching the budget is verdictUnknown - the answer exists somewhere
// the scan did not reach, and reporting that as "no UID" would let a
// contradictory reference resolve.
func scanLines(path string, decide func(line string) (string, bool)) (string, scanVerdict) {
	file, err := os.Open(path)
	if err != nil {
		return "", verdictUnknown
	}
	defer file.Close()
	reader := bufio.NewReaderSize(io.LimitReader(file, int64(scanBudget)+1), 64<<10)
	consumed := 0
	for {
		line, readErr := reader.ReadString('\n')
		consumed += len(line)
		truncated := consumed > scanBudget
		settledLine := strings.HasSuffix(line, "\n") || (readErr == io.EOF && line != "" && !truncated)
		if settledLine {
			if value, settled := decide(strings.TrimRight(line, "\r\n")); settled {
				if value == "" {
					return "", verdictNone
				}
				return value, verdictDeclared
			}
		}
		switch {
		case truncated:
			return "", verdictUnknown
		case readErr == io.EOF:
			return "", verdictNone
		case readErr != nil:
			return "", verdictUnknown
		}
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

func aliasDigest(aliases *Aliases) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("godotid-aliases-v2"))
	for _, project := range aliases.Projects {
		_, _ = digest.Write([]byte("project\x00" + project + "\x00"))
	}
	// An unreadable file changes how every reference resolves, so a file that
	// becomes readable has to reparse the files it affects.
	for _, unknown := range aliases.Unknown {
		_, _ = digest.Write([]byte("unknown\x00" + unknown + "\x00"))
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
