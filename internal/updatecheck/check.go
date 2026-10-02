package updatecheck

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"
)

// EnvDisable turns the automatic check off entirely.
const EnvDisable = "GRAFO_NO_UPDATE_CHECK"

const (
	// defaultMaxAge is how long one feed lookup is reused. A day keeps an
	// unauthenticated GitHub client far below its hourly request allowance even
	// across many repositories.
	defaultMaxAge = 24 * time.Hour
	// defaultFetchTimeout bounds the background lookup.
	defaultFetchTimeout = 3 * time.Second
	// defaultWaitBudget is how long a finished command waits for a lookup that
	// is still in flight. The wait exists only so the first run on a fast
	// network already reports the release instead of deferring it to the next
	// command; anything slower is left to the cache.
	defaultWaitBudget = 200 * time.Millisecond
)

// Checker decides whether a newer release exists and renders the advisory. Its
// clock, feed and cache location are fields so no test reaches the network or
// the real cache directory.
type Checker struct {
	CurrentVersion string
	ExecutablePath string
	Method         Method
	Feed           Feed
	CachePath      string
	Now            func() time.Time
	MaxAge         time.Duration
	FetchTimeout   time.Duration
	WaitBudget     time.Duration
}

// NewChecker builds the production checker for the running binary. A cache path
// that cannot be resolved yields a checker that still works, lookup by lookup,
// without remembering anything.
func NewChecker(currentVersion string) *Checker {
	executablePath, err := os.Executable()
	if err != nil {
		executablePath = ""
	}
	cachePath, err := CachePath()
	if err != nil {
		cachePath = ""
	}
	return &Checker{
		CurrentVersion: currentVersion,
		ExecutablePath: executablePath,
		Method:         Detect(executablePath),
		Feed:           GitHubFeed{Client: &http.Client{Timeout: defaultFetchTimeout}},
		CachePath:      cachePath,
	}
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) maxAge() time.Duration {
	if c.MaxAge > 0 {
		return c.MaxAge
	}
	return defaultMaxAge
}

func (c *Checker) fetchTimeout() time.Duration {
	if c.FetchTimeout > 0 {
		return c.FetchTimeout
	}
	return defaultFetchTimeout
}

func (c *Checker) waitBudget() time.Duration {
	if c.WaitBudget > 0 {
		return c.WaitBudget
	}
	return defaultWaitBudget
}

// Lookup is an in-progress or already-answered check. A nil Lookup renders no
// notice, so callers that skipped the check need no special case.
type Lookup struct {
	checker *Checker
	known   string
	fetched chan string
}

// Start begins a check. It reads the remembered lookup, which costs one small
// local file read, and starts a background request only when that record is
// missing or older than MaxAge. The command itself is never blocked.
func (c *Checker) Start(ctx context.Context) *Lookup {
	if c == nil {
		return nil
	}
	lookup := &Lookup{checker: c}
	remembered, present := readState(c.CachePath)
	if present {
		lookup.known = remembered.LatestVersion
		if c.now().Sub(remembered.CheckedAt) < c.maxAge() {
			return lookup
		}
	}
	// The attempt is remembered before it is made, carrying forward whatever
	// version was already known. Without this, a lookup that outlives the
	// command it was started from would leave the record stale and every
	// subsequent command would issue its own request.
	writeState(c.CachePath, state{CheckedAt: c.now(), LatestVersion: lookup.known})
	lookup.fetched = make(chan string, 1)
	go c.fetch(ctx, lookup.fetched)
	return lookup
}

// fetch performs one lookup and remembers the answer. Failures are dropped: the
// recorded attempt above already keeps the next command from retrying
// immediately, and there is no action for the caller to take.
func (c *Checker) fetch(ctx context.Context, answer chan<- string) {
	defer close(answer)
	latest, err := c.LatestRelease(ctx)
	if err != nil {
		return
	}
	writeState(c.CachePath, state{CheckedAt: c.now(), LatestVersion: latest})
	answer <- latest
}

// LatestRelease reads the feed directly, bypassing the cache. It is the path
// `grafo version --check` takes, and the only path that reports its errors.
func (c *Checker) LatestRelease(ctx context.Context) (string, error) {
	feed := c.Feed
	if feed == nil {
		feed = GitHubFeed{Client: &http.Client{Timeout: c.fetchTimeout()}}
	}
	fetchCtx, cancel := context.WithTimeout(ctx, c.fetchTimeout())
	defer cancel()
	return feed.LatestRelease(fetchCtx)
}

// Notice renders the advisory for a finished command, or an empty string when
// there is nothing to say. It waits at most WaitBudget for a lookup that is
// still in flight.
func (l *Lookup) Notice() string {
	if l == nil || l.checker == nil {
		return ""
	}
	latest := l.known
	if l.fetched != nil {
		timer := time.NewTimer(l.checker.waitBudget())
		defer timer.Stop()
		select {
		case fetched, ok := <-l.fetched:
			// An absent remembered version is not "newer than nothing", so the
			// first lookup on a machine has to be adopted outright.
			if ok && (Comparable(latest) == "" || Newer(latest, fetched)) {
				latest = fetched
			}
		case <-timer.C:
		}
	}
	if !Newer(l.checker.CurrentVersion, latest) {
		return ""
	}
	return Notice(l.checker.CurrentVersion, latest, l.checker.Method, l.checker.ExecutablePath)
}

// Disabled reports whether the environment turns the automatic check off.
// Continuous integration is excluded by default because a notice there reaches
// a log nobody upgrades from.
func Disabled(lookupEnv func(string) (string, bool)) bool {
	return envTrue(lookupEnv, EnvDisable) || envTrue(lookupEnv, "CI")
}

// envTrue treats a set variable as on unless it holds an explicit negative, so
// `GRAFO_NO_UPDATE_CHECK=1` disables the check while `CI=false` does not. An
// empty value reads as absent, because it almost always comes from expanding a
// variable that was never set rather than from a deliberate opt-in.
func envTrue(lookupEnv func(string) (string, bool), name string) bool {
	value, present := lookupEnv(name)
	if !present {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// UpgradeHint names the command that moves this install to latest.
func (c *Checker) UpgradeHint(latest string) string {
	return Hint(c.Method, latest, c.ExecutablePath)
}
