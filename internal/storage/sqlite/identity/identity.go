// Package identity encodes a graph identity for storage.
//
// A graph identity is text: graph.StableID produces a prefix, a colon, and the
// first 20 hex characters of a SHA-256. Stored as text that is 22 bytes for a
// one-letter prefix, and identity columns are the widest thing in the index --
// an edge row carries three of them and each of its indexes carries two or
// three more. Half of those 22 bytes are hex digits spelling out 10 bytes of
// digest.
//
// So an identity is stored as a BLOB holding the digest raw, which takes a
// 12,744-file index from 1909 MiB to 1584 MiB. Two bytes of that encoding are
// deliberate overhead and both buy correctness:
//
// The separator is kept, because without it nothing marks where the prefix
// ends: a digest byte can be an ASCII letter -- 0x6e is 'n' -- so a decoder
// that scanned leading letters would mis-split real identities. Prefixes are
// also not one letter wide; the repository node is "repo:".
//
// The leading tag is kept because the encoding has to be total. Not every
// identity is StableID output: test fixtures use identities like "a" and "a-b",
// and the encoding has to return them unchanged rather than corrupt them or
// fail. The tag says which of the two forms a value is in, so decoding needs no
// guess. Guessing was tried and is not possible: a literal whose tail happens to
// be ten bytes after a colon is indistinguishable from a compact one.
//
// Ordering is preserved for every identity a real index holds. Compact values
// share a tag and then compare prefix-first and digest-second, and for
// equal-length lowercase hex, string order and raw-byte order agree -- so
// identity columns can still be ORDER BY tiebreaks and pagination cursors. The
// empty identity encodes to an empty BLOB, which keeps the "no target" sentinel
// distinct from every present identity and still sorts before all of them.
package identity

import (
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	// compactTag marks a value stored as prefix, separator, and raw digest.
	compactTag = 0x00
	// literalTag marks a value stored as its own bytes, for an identity that is
	// not StableID output.
	literalTag = 0x01
	// digestHexLength is how many hex characters graph.StableID emits.
	digestHexLength = 20
)

// Key is a graph identity as it crosses the storage boundary. It is the Go type
// sqlc generates for every identity column, so the encoding happens in Value and
// Scan rather than at each of the call sites that bind or read one.
type Key string

// Value encodes the identity for storage.
func (k Key) Value() (driver.Value, error) { return Encode(string(k)), nil }

// Scan decodes a stored identity.
//
// Text is accepted as well as BLOB, because an index may be read before its
// migration has run -- a migration's own statements see the old column -- and
// because SQLite is dynamically typed, so a value's storage class is a property
// of the value and not of the column it sits in.
func (k *Key) Scan(source any) error {
	switch value := source.(type) {
	case nil:
		*k = ""
	case []byte:
		*k = Key(Decode(value))
	case string:
		*k = Key(value)
	default:
		return fmt.Errorf("identity: cannot scan %T", source)
	}
	return nil
}

// Encode returns the stored form of an identity.
func Encode(id string) []byte {
	if id == "" {
		return []byte{}
	}
	if prefix, digest, ok := splitCompact(id); ok {
		raw, err := hex.DecodeString(digest)
		if err == nil {
			encoded := make([]byte, 0, 2+len(prefix)+len(raw))
			encoded = append(encoded, compactTag)
			encoded = append(encoded, prefix...)
			encoded = append(encoded, ':')
			return append(encoded, raw...)
		}
	}
	return append([]byte{literalTag}, id...)
}

// Decode returns the identity a stored value holds. It is the exact inverse of
// Encode: every identity of a real index, and every shape a fixture uses, comes
// back byte for byte.
func Decode(stored []byte) string {
	if len(stored) == 0 {
		return ""
	}
	if stored[0] == literalTag {
		return string(stored[1:])
	}
	body := stored[1:]
	separator := strings.IndexByte(string(body), ':')
	if separator < 0 {
		// Not a shape Encode produces. Reporting the bytes is better than
		// dropping them: the caller sees a corrupt identity rather than an empty
		// one it would treat as a sentinel.
		return string(body)
	}
	return string(body[:separator]) + ":" + hex.EncodeToString(body[separator+1:])
}

// splitCompact reports whether an identity is StableID output, which is a prefix
// carrying no separator of its own followed by exactly the digest StableID emits.
func splitCompact(id string) (prefix, digest string, ok bool) {
	separator := strings.IndexByte(id, ':')
	if separator < 0 || len(id)-separator-1 != digestHexLength {
		return "", "", false
	}
	return id[:separator], id[separator+1:], true
}
