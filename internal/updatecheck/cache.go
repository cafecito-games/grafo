package updatecheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnvCachePath overrides where the last lookup is remembered. Tests and
// sandboxed environments set it so a check never touches the real cache.
const EnvCachePath = "GRAFO_UPDATE_CACHE"

// state is the whole persisted record: when the feed was last read, and what it
// said. It is deliberately not keyed by the running version, so downgrading
// does not hide a release the cache already knows about.
type state struct {
	CheckedAt     time.Time `json:"checked_at"`
	LatestVersion string    `json:"latest_version"`
}

// CachePath is where the last lookup is remembered. It sits in the user cache
// directory beside the embedding cache so one check serves every repository on
// the machine.
func CachePath() (string, error) {
	if value, present := os.LookupEnv(EnvCachePath); present {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s is set but empty", EnvCachePath)
		}
		return filepath.Abs(value)
	}
	root, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory (set %s to override): %w", EnvCachePath, err)
	}
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("resolve user cache directory: empty path (set %s to override)", EnvCachePath)
	}
	return filepath.Join(root, "grafo", "update-check.json"), nil
}

// readState loads the remembered lookup. A missing, unreadable or corrupt file
// is reported as an absent record rather than an error: the only consequence is
// one more feed request.
//
// A record with a timestamp but no version is valid and means the feed was
// consulted and reported nothing usable. Rejecting it would make the attempt
// written before a lookup unreadable, and every later command would issue its
// own request until one succeeded.
func readState(path string) (state, bool) {
	if path == "" {
		return state{}, false
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return state{}, false
	}
	var loaded state
	if err := json.Unmarshal(contents, &loaded); err != nil {
		return state{}, false
	}
	if loaded.CheckedAt.IsZero() {
		return state{}, false
	}
	if Comparable(loaded.LatestVersion) == "" {
		loaded.LatestVersion = ""
	}
	return loaded, true
}

// writeState remembers one lookup. Every failure is dropped, because an
// unwritable cache directory degrades the check to one request per command and
// nothing more; it is not a condition the caller can act on.
func writeState(path string, value state) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	contents, err := json.Marshal(value)
	if err != nil {
		return
	}
	// The write is atomic so a cancelled command never leaves a truncated
	// record that the next run has to discard.
	temporary, err := os.CreateTemp(filepath.Dir(path), ".update-check-*")
	if err != nil {
		return
	}
	temporaryPath := temporary.Name()
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
	}
}
