// Package agentguide owns Grafo's canonical agent guidance: the embedded
// structural tool-routing and repository-setup playbooks, their independent
// format versions, and the exact markers that make installed copies provably
// Grafo-owned.
//
// The package renders the same canonical text into the two surfaces installers
// need — an isolated Grafo-owned skill file and a delimited managed block inside
// a user-authored instruction file — and never parses or rewrites anything
// outside its own markers.
package agentguide

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
)

// Version is the guidance format version. Bumping it makes every installed copy
// stale, so `grafo install` replaces older Grafo-owned content exactly.
const Version = "4"

// SetupVersion is the repository-setup skill format version. It is independent
// from Version because either installed skill may evolve without making the
// other stale.
const SetupVersion = "2"

// Name is the stable identity Grafo installs guidance under.
const Name = "grafo"

// SetupName is the stable identity of the installed repository onboarding skill.
const SetupName = "grafo-setup"

// Description is the one-line summary clients show when listing skills.
const Description = "Route structural code questions through Grafo's semantic graph: " +
	"confirm the branch is indexed, resolve symbols before searching text, find reusable " +
	"code before adding code, and run bidirectional impact before a behaviour-changing edit."

// SetupDescription tells skill-capable clients when to invoke the repository
// onboarding workflow.
const SetupDescription = "Set up or improve Grafo for a repository: inspect project boundaries, " +
	"transports, serialization, semantic application wrappers, SQL, and test conventions; update only supported " +
	"grafo.yaml settings; index the project; and verify graph coverage without inventing configuration."

// BeginMarker and EndMarker delimit the managed block. They are matched exactly;
// ownership is never inferred from a substring of the guidance body itself.
const (
	BeginMarker = "<!-- BEGIN grafo-guidance -->"
	EndMarker   = "<!-- END grafo-guidance -->"
)

//go:embed guide.md
var canonical string

//go:embed setup.md
var setupCanonical string

// versionComment is the in-body provenance line. It is part of the digested
// content, so a version bump changes every digest.
func versionComment() string {
	return fmt.Sprintf("<!-- grafo-guidance version %s; managed by `grafo install`; edits are replaced -->", Version)
}

// Text returns the canonical guidance with its version marker.
func Text() string {
	body := strings.TrimRight(canonical, "\n")
	heading, rest, found := strings.Cut(body, "\n")
	if !found {
		return heading + "\n\n" + versionComment() + "\n"
	}
	return heading + "\n\n" + versionComment() + "\n" + rest + "\n"
}

// Skill renders the isolated Grafo-owned skill file: the whole file belongs to
// Grafo, so it carries frontmatter and nothing of the user's.
//
// The description contains ": ", which is not a legal YAML plain scalar, so it is
// emitted as a double-quoted scalar. An unquoted value makes the frontmatter
// unparseable and the skill undiscoverable.
func Skill() string {
	return renderSkill(Name, Description, Text())
}

// SetupSkill renders the isolated Grafo-owned repository-setup skill file.
func SetupSkill() string {
	return renderSkill(SetupName, SetupDescription, setupText())
}

func renderSkill(name, description, text string) string {
	var builder strings.Builder
	builder.WriteString("---\n")
	builder.WriteString("name: " + name + "\n")
	builder.WriteString("description: " + quoteYAML(description) + "\n")
	builder.WriteString("---\n\n")
	builder.WriteString(text)
	return builder.String()
}

func setupText() string {
	body := strings.TrimRight(setupCanonical, "\n")
	return body + "\n\n" + setupVersionComment() + "\n"
}

func setupVersionComment() string {
	return fmt.Sprintf("<!-- grafo-setup version %s; managed by `grafo install`; edits are replaced -->", SetupVersion)
}

// quoteYAML renders a value as a YAML double-quoted scalar, which accepts any
// printable content once backslashes and quotes are escaped.
func quoteYAML(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	return `"` + escaped + `"`
}

// Block renders the managed block that may be embedded in a user-authored
// instruction file.
func Block() string {
	return BeginMarker + "\n" + Text() + EndMarker + "\n"
}

// Change reports what an upsert or removal did to a document.
type Change int

const (
	// Unchanged means the document already holds exactly the current content.
	Unchanged Change = iota
	// Added means a managed block was appended.
	Added
	// Updated means an existing Grafo-owned block was replaced.
	Updated
	// Removed means an existing Grafo-owned block was deleted.
	Removed
)

func (c Change) String() string {
	switch c {
	case Added:
		return "added"
	case Updated:
		return "updated"
	case Removed:
		return "removed"
	default:
		return "unchanged"
	}
}

// ConflictError reports that a document's Grafo markers are partial, duplicated,
// or out of order. Callers must leave the document byte-identical.
type ConflictError struct{ Reason string }

func (e *ConflictError) Error() string {
	return "grafo-guidance markers are " + e.Reason + "; refusing to rewrite the file"
}

// span locates the single managed block in a document.
type span struct {
	begin, end int // byte offsets of the block, end exclusive
	present    bool
}

func locate(document string) (span, error) {
	begins := strings.Count(document, BeginMarker)
	ends := strings.Count(document, EndMarker)
	switch {
	case begins == 0 && ends == 0:
		return span{}, nil
	case begins > 1 || ends > 1:
		return span{}, &ConflictError{Reason: "duplicated"}
	case begins != ends:
		return span{}, &ConflictError{Reason: "incomplete"}
	}
	begin := strings.Index(document, BeginMarker)
	end := strings.Index(document, EndMarker)
	if end < begin {
		return span{}, &ConflictError{Reason: "out of order"}
	}
	return span{begin: begin, end: end + len(EndMarker), present: true}, nil
}

// UpsertBlock returns the document with exactly one current managed block,
// leaving every byte outside the markers untouched.
func UpsertBlock(document string) (string, Change, error) {
	found, err := locate(document)
	if err != nil {
		return document, Unchanged, err
	}
	block := Block()
	if !found.present {
		prefix := document
		if prefix != "" {
			prefix = strings.TrimRight(prefix, "\n") + "\n\n"
		}
		return prefix + block, Added, nil
	}
	if document[found.begin:found.end] == strings.TrimSuffix(block, "\n") {
		return document, Unchanged, nil
	}
	return document[:found.begin] + strings.TrimSuffix(block, "\n") + document[found.end:], Updated, nil
}

// RemoveBlock deletes a Grafo-owned block and restores the surrounding text,
// leaving unrelated content byte-equivalent. It is idempotent.
func RemoveBlock(document string) (string, Change, error) {
	found, err := locate(document)
	if err != nil {
		return document, Unchanged, err
	}
	if !found.present {
		return document, Unchanged, nil
	}
	prefix := strings.TrimRight(document[:found.begin], "\n")
	suffix := strings.TrimLeft(document[found.end:], "\n")
	switch {
	case prefix == "" && suffix == "":
		return "", Removed, nil
	case prefix == "":
		return suffix, Removed, nil
	case suffix == "":
		return prefix + "\n", Removed, nil
	default:
		return prefix + "\n" + suffix, Removed, nil
	}
}

// HasBlock reports whether a document already carries a Grafo-owned block.
func HasBlock(document string) bool {
	found, err := locate(document)
	return err == nil && found.present
}

// Owns reports whether a document carries Grafo's ownership marker, for any
// guidance version. Ownership is never inferred from the guidance prose.
func Owns(document string) bool {
	return strings.Contains(document, ownershipMarker)
}

// OwnsSetup reports whether a document carries the setup skill's ownership
// marker, for any setup format version.
func OwnsSetup(document string) bool {
	return strings.Contains(document, SetupMarker)
}

// ownershipMarker is the stable prefix of the in-body provenance line.
const ownershipMarker = "<!-- grafo-guidance version "

// SetupMarker is recorded in install receipts for the repository-setup skill.
const SetupMarker = "<!-- grafo-setup version "

// Digest labels the SHA-256 of installed content so a receipt can prove that an
// artifact is still exactly what Grafo wrote.
func Digest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}
