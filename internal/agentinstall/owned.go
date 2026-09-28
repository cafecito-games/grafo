package agentinstall

import (
	"fmt"
	"slices"
	"time"

	"github.com/cafecito-games/grafo/internal/agentguide"
	"github.com/cafecito-games/grafo/internal/version"
)

// ServiceOwner labels the background-service artifacts Grafo installs in the
// shared receipt ledger. Service definitions are recorded beside client
// registrations so there is exactly one ownership ledger on disk, and one place
// that can prove Grafo wrote a file before it is rewritten or removed.
const ServiceOwner = "grafo-service"

// ConfigHomePath resolves a path inside the XDG-style configuration directory
// ($XDG_CONFIG_HOME or ~/.config, %APPDATA% on Windows). Callers outside this
// package use it so the service registry lands in the same configuration root
// the installer already writes to.
func ConfigHomePath(reader Reader, elements ...string) (string, error) {
	return configHomePath(reader, elements...)
}

// HomeDirPath resolves a path relative to the user's home directory.
func HomeDirPath(reader Reader, elements ...string) (string, error) {
	return homePath(reader, elements...)
}

// JoinPath joins path elements with the target platform's separator.
func JoinPath(goos string, elements ...string) string { return joinPath(goos, elements...) }

// ParentPath returns the directory containing path, or "" when there is none.
func ParentPath(goos, path string) string { return parentPath(goos, path) }

// ResolvePath resolves every symlinked parent of path and returns the real
// location, leaving the final element unresolved.
func ResolvePath(reader Reader, path string) (string, error) { return resolvePath(reader, path) }

// CheckUserConfigRoot refuses paths that escape the user's configuration roots.
// It is the single containment boundary for everything Grafo writes outside a
// repository, shared by guidance installation and by service definitions.
func CheckUserConfigRoot(reader Reader, path string) error {
	return checkUserConfigRoot(reader, path)
}

// ResolveUserConfigPath returns the physical direct-write target after proving
// it remains inside the configured user roots. The leaf stays unresolved.
func ResolveUserConfigPath(reader Reader, path string) (string, error) {
	return resolveUserConfigPath(reader, path)
}

// RevalidateUserConfigPath checks that a direct-write target still resolves to
// the physical path captured during planning and that its leaf is not a symlink.
func RevalidateUserConfigPath(reader Reader, path, expected string) error {
	return revalidateUserConfigPath(reader, path, expected)
}

// Digest labels content the way receipts record it.
func Digest(contents string) string { return agentguide.Digest(contents) }

// OwnedFile reports the receipt Grafo holds for one non-client artifact, such as
// a generated service definition. The boolean reports whether a receipt exists
// at all; a missing receipt is not an error.
func OwnedFile(reader Reader, owner, kind string) (Receipt, bool, error) {
	store, err := loadReceipts(reader)
	if err != nil {
		return Receipt{}, false, err
	}
	evidence := store.lookup(owner, kind)
	return evidence.receipt, evidence.found, nil
}

// ProvesFile reports whether a receipt proves Grafo wrote exactly these bytes at
// exactly this resolved path.
func ProvesFile(reader Reader, receipt Receipt, path, contents string) bool {
	return ownership{receipt: receipt, found: receipt.Target != "", reader: reader}.provesFile(path, contents)
}

// ProvesContents reports whether a receipt's digest proves Grafo wrote exactly
// these bytes. It says nothing about location: pair it with OwnedFileAt, which
// binds a receipt to one resolved path.
func ProvesContents(receipt Receipt, contents string) bool {
	return receipt.Digest != "" && receipt.Digest == agentguide.Digest(contents)
}

// OwnedFileAt reports the receipt Grafo holds for one non-client artifact, but
// only when that receipt covers this exact location. Targets are compared after
// resolving every symlinked parent, so a receipt written for one path can never
// authorize a mutation at a different path that merely resolves nearby, and a
// path spelled differently from the recorded target still matches when both name
// the same file.
//
// Keying ownership to the artifact kind alone is the defect PR #53 fixed for hook
// receipts; this is the same contract for generated service definitions.
func OwnedFileAt(reader Reader, owner, kind, path string) (Receipt, bool, error) {
	receipt, found, err := OwnedFile(reader, owner, kind)
	if err != nil || !found {
		return Receipt{}, false, err
	}
	if !(ownership{receipt: receipt, found: true, reader: reader}).provesPath(path) {
		return Receipt{}, false, nil
	}
	return receipt, true, nil
}

// RecordOwnedFile records a non-client artifact Grafo has just written. The
// resolved target must be the identity captured and checked before that write;
// it is revalidated here so a later parent retarget cannot be recorded as the
// destination of an earlier mutation.
func RecordOwnedFile(env Environment, owner, kind, target, resolvedTarget, contents string) error {
	reader := Reader(env)
	store, err := loadReceipts(reader)
	if err != nil {
		return err
	}
	if err = revalidateUserConfigPath(reader, target, resolvedTarget); err != nil {
		return fmt.Errorf("record owned file %s: %w", target, err)
	}
	store.recordOwnedFile(owner, kind, target, resolvedTarget, agentguide.Digest(contents))
	return store.save(env)
}

// ForgetOwnedFile drops the receipt for an artifact Grafo has just removed, or
// for one the user deleted, so stale evidence can never authorize a later write.
func ForgetOwnedFile(env Environment, owner, kind string) error {
	store, err := loadReceipts(Reader(env))
	if err != nil {
		return err
	}
	store.drop(Client{Name: owner}, kind)
	return store.save(env)
}

// recordOwnedFile stores a receipt for an artifact that belongs to no MCP
// client, so it carries no guidance version or marker.
func (s *receiptStore) recordOwnedFile(owner, kind, target, resolvedTarget, digest string) {
	entry := Receipt{
		Client:         owner,
		Kind:           kind,
		Target:         target,
		ResolvedTarget: resolvedTarget,
		Digest:         digest,
		Grafo:          version.Value,
		Updated:        now().Format(time.RFC3339),
	}
	if index := slices.IndexFunc(s.entries, func(existing Receipt) bool {
		return existing.Client == entry.Client && existing.Kind == entry.Kind
	}); index >= 0 {
		s.entries[index] = entry
	} else {
		s.entries = append(s.entries, entry)
	}
	s.dirty = true
}
