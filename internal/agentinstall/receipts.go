package agentinstall

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cafecito-games/grafo/internal/agentguide"
	"github.com/cafecito-games/grafo/internal/version"
)

// receiptFormat is the on-disk format marker of the receipt file. It is checked
// exactly, so a future format is reported instead of being overwritten.
const receiptFormat = "grafo.install.receipts/1"

// now is the clock, replaced in tests so receipts are comparable.
var now = func() time.Time { return time.Now().UTC() }

// Receipt records one artifact Grafo installed, so a later uninstall or upgrade
// can prove ownership instead of guessing from file contents alone.
type Receipt struct {
	Client string `json:"client"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Digest string `json:"digest,omitempty"`
	// Commands are the exact command lines Grafo installed for a hook artifact,
	// in hookPhases order. Removal matches them exactly, so a user-authored
	// entry that merely resembles one is never claimed.
	Commands []string `json:"commands,omitempty"`
	Marker   string   `json:"marker,omitempty"`
	Guidance string   `json:"guidance_version,omitempty"`
	Grafo    string   `json:"grafo_version"`
	Updated  string   `json:"updated_at"`
}

// receiptFile is the document stored under the Grafo configuration directory.
type receiptFile struct {
	Format   string    `json:"format"`
	Receipts []Receipt `json:"receipts"`
}

// receiptStore is the loaded receipt file plus the pending changes of one run.
type receiptStore struct {
	path    string
	entries []Receipt
	dirty   bool
}

// receiptPath resolves the receipt file inside the Grafo configuration directory.
func receiptPath(reader Reader) (string, error) {
	return configHomePath(reader, serverName, "installed-artifacts.json")
}

// loadReceipts reads the receipt file. A missing file yields an empty store; a
// malformed or unknown-format file is an error, so the run reports it and leaves
// the file byte-identical.
func loadReceipts(reader Reader) (*receiptStore, error) {
	path, err := receiptPath(reader)
	if err != nil {
		return nil, err
	}
	store := &receiptStore{path: path}
	data, err := reader.ReadFile(path)
	if err != nil {
		return store, nil
	}
	if strings.TrimSpace(string(data)) == "" {
		return store, nil
	}
	var document receiptFile
	if err = json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("grafo receipt file %s is malformed: %w; move it aside to continue", path, err)
	}
	if document.Format != receiptFormat {
		return nil, fmt.Errorf("grafo receipt file %s uses unsupported format %q (expected %q)",
			path, document.Format, receiptFormat)
	}
	store.entries = document.Receipts
	return store, nil
}

// lookup returns the receipt evidence for one client artifact.
func (s *receiptStore) lookup(client, kind string) ownership {
	index := slices.IndexFunc(s.entries, func(entry Receipt) bool {
		return entry.Client == client && entry.Kind == kind
	})
	if index == -1 {
		return ownership{}
	}
	return ownership{receipt: s.entries[index], found: true}
}

// ownership is the receipt evidence Grafo has for one installed artifact.
// Ownership of whole-file and hook artifacts is proven from a receipt, never
// from a marker substring alone, so uninstall can never delete or rewrite
// content Grafo cannot prove it wrote.
type ownership struct {
	receipt Receipt
	found   bool
}

// provesFile reports whether a receipt proves Grafo wrote exactly these bytes at
// exactly this path.
func (o ownership) provesFile(path, contents string) bool {
	return o.found && o.receipt.Target == path && o.receipt.Digest != "" &&
		o.receipt.Digest == agentguide.Digest(contents)
}

// provesPath reports whether a receipt claims this exact target at all.
func (o ownership) provesPath(path string) bool {
	return o.found && o.receipt.Target == path
}

// commandsFor returns the exact command lines a receipt records, but only when
// the receipt was written for this very target. A receipt from an old
// configuration location must never authorize a mutation in a different settings
// file: the identical entry there may be the user's own.
func (o ownership) commandsFor(path string) []string {
	if !o.provesPath(path) {
		return nil
	}
	return o.receipt.Commands
}

// record stores a receipt for an artifact whose mutation already succeeded.
func (s *receiptStore) record(client Client, kind, target, digest string, commands []string) {
	entry := Receipt{
		Client:   client.Name,
		Kind:     kind,
		Target:   target,
		Digest:   digest,
		Commands: commands,
		Grafo:    version.Value,
		Updated:  now().Format(time.RFC3339),
	}
	if kind != KindMCP {
		entry.Marker = agentguide.BeginMarker
		entry.Guidance = agentguide.Version
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

// drop forgets an artifact Grafo has just removed.
func (s *receiptStore) drop(client Client, kind string) {
	filtered := slices.DeleteFunc(slices.Clone(s.entries), func(entry Receipt) bool {
		return entry.Client == client.Name && entry.Kind == kind
	})
	if len(filtered) == len(s.entries) {
		return
	}
	s.entries = filtered
	s.dirty = true
}

// save writes the receipt file atomically, but only when a mutation changed it.
func (s *receiptStore) save(env Environment) error {
	if !s.dirty {
		return nil
	}
	entries := slices.Clone(s.entries)
	slices.SortStableFunc(entries, func(left, right Receipt) int {
		if order := strings.Compare(left.Client, right.Client); order != 0 {
			return order
		}
		return strings.Compare(left.Kind, right.Kind)
	})
	if entries == nil {
		entries = []Receipt{}
	}
	data, err := json.MarshalIndent(receiptFile{Format: receiptFormat, Receipts: entries}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if directory := parentPath(env.GOOS(), s.path); directory != "" {
		if err = env.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create grafo configuration directory %s: %w", directory, err)
		}
	}
	if err = env.WriteFileAtomic(s.path, data, 0o644); err != nil {
		return fmt.Errorf("write grafo receipt file %s: %w", s.path, err)
	}
	s.dirty = false
	return nil
}

// Receipts reports the artifacts Grafo has installed, newest content first in
// deterministic client and kind order. It never mutates anything.
func Receipts(reader Reader) ([]Receipt, error) {
	store, err := loadReceipts(reader)
	if err != nil {
		return nil, err
	}
	entries := slices.Clone(store.entries)
	slices.SortStableFunc(entries, func(left, right Receipt) int {
		if order := strings.Compare(left.Client, right.Client); order != 0 {
			return order
		}
		return strings.Compare(left.Kind, right.Kind)
	})
	return entries, nil
}
