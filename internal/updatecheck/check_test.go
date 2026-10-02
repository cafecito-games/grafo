package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cafecito-games/grafo/internal/testtemp"
	"time"
)

// stubFeed answers from memory and counts its calls, so a test can assert that
// a cached lookup issued no request at all.
type stubFeed struct {
	tag   string
	err   error
	block chan struct{}
	calls atomic.Int32
}

func (f *stubFeed) LatestRelease(ctx context.Context) (string, error) {
	f.calls.Add(1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.err != nil {
		return "", f.err
	}
	return f.tag, nil
}

// awaitLookup releases a blocked lookup at the end of the test and waits for it
// to finish. The wait is on the lookup's own channel, which is closed only
// after the cache has been written, so the write cannot race the removal of the
// temporary cache directory. It must be registered after the checker is built
// so it runs before that directory's own cleanup.
func awaitLookup(t *testing.T, lookup *Lookup, release chan struct{}) {
	t.Helper()
	t.Cleanup(func() {
		close(release)
		for range lookup.fetched {
		}
	})
}

func newChecker(t *testing.T, feed Feed, current string) *Checker {
	t.Helper()
	return &Checker{
		CurrentVersion: current,
		ExecutablePath: "/opt/homebrew/bin/grafo",
		Method:         MethodHomebrew,
		Feed:           feed,
		CachePath:      filepath.Join(testtemp.Dir(t), "update-check.json"),
	}
}

func TestStartReportsANewerRelease(t *testing.T) {
	feed := &stubFeed{tag: "v0.4.2"}
	checker := newChecker(t, feed, "0.4.1")

	notice := checker.Start(context.Background()).Notice()
	if !strings.Contains(notice, "0.4.2 is available") {
		t.Fatalf("notice = %q, want it to announce 0.4.2", notice)
	}
	if !strings.Contains(notice, "brew upgrade --cask grafo") {
		t.Fatalf("notice = %q, want the homebrew upgrade command", notice)
	}
}

func TestStartSaysNothingWhenCurrent(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		current string
		tag     string
	}{
		{name: "same version", current: "0.4.2", tag: "v0.4.2"},
		{name: "ahead of the newest release", current: "0.5.0", tag: "v0.4.2"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			checker := newChecker(t, &stubFeed{tag: testCase.tag}, testCase.current)
			if notice := checker.Start(context.Background()).Notice(); notice != "" {
				t.Fatalf("notice = %q, want none", notice)
			}
		})
	}
}

func TestStartSurvivesAFailedLookup(t *testing.T) {
	feed := &stubFeed{err: errors.New("no network")}
	checker := newChecker(t, feed, "0.4.1")

	if notice := checker.Start(context.Background()).Notice(); notice != "" {
		t.Fatalf("notice = %q, want none after a failed lookup", notice)
	}
}

// TestStartReusesARememberedLookup covers the ordinary case: a check that ran
// within the last day issues no request.
func TestStartReusesARememberedLookup(t *testing.T) {
	feed := &stubFeed{tag: "v0.9.0"}
	checker := newChecker(t, feed, "0.4.1")
	writeState(checker.CachePath, state{CheckedAt: time.Now(), LatestVersion: "v0.4.2"})

	notice := checker.Start(context.Background()).Notice()
	if !strings.Contains(notice, "0.4.2 is available") {
		t.Fatalf("notice = %q, want the remembered 0.4.2", notice)
	}
	if calls := feed.calls.Load(); calls != 0 {
		t.Fatalf("feed called %d times, want 0 for a fresh record", calls)
	}
}

func TestStartRefreshesAnExpiredLookup(t *testing.T) {
	feed := &stubFeed{tag: "v0.4.3"}
	checker := newChecker(t, feed, "0.4.1")
	checker.MaxAge = time.Hour
	writeState(checker.CachePath, state{
		CheckedAt:     time.Now().Add(-2 * time.Hour),
		LatestVersion: "v0.4.2",
	})

	notice := checker.Start(context.Background()).Notice()
	if !strings.Contains(notice, "0.4.3 is available") {
		t.Fatalf("notice = %q, want the refreshed 0.4.3", notice)
	}
	if calls := feed.calls.Load(); calls != 1 {
		t.Fatalf("feed called %d times, want 1 for an expired record", calls)
	}
}

// TestStartRemembersASuccessfulLookup covers the record the next command reads.
func TestStartRemembersASuccessfulLookup(t *testing.T) {
	checker := newChecker(t, &stubFeed{tag: "v0.4.2"}, "0.4.1")
	checker.Start(context.Background()).Notice()

	remembered, present := readState(checker.CachePath)
	if !present {
		t.Fatal("no lookup was remembered")
	}
	if remembered.LatestVersion != "v0.4.2" {
		t.Fatalf("remembered %q, want v0.4.2", remembered.LatestVersion)
	}
}

// TestStartBoundsTheWaitForASlowLookup is the latency guarantee: a feed that
// never answers must not hold the command open beyond the wait budget.
func TestStartBoundsTheWaitForASlowLookup(t *testing.T) {
	released := make(chan struct{})
	feed := &stubFeed{tag: "v0.4.2", block: released}
	checker := newChecker(t, feed, "0.4.1")
	checker.WaitBudget = 20 * time.Millisecond
	checker.FetchTimeout = 10 * time.Second

	lookup := checker.Start(context.Background())
	awaitLookup(t, lookup, released)
	started := time.Now()
	notice := lookup.Notice()
	elapsed := time.Since(started)

	if notice != "" {
		t.Fatalf("notice = %q, want none while the lookup is still in flight", notice)
	}
	if elapsed > time.Second {
		t.Fatalf("Notice waited %s, want it bounded by the %s budget", elapsed, checker.WaitBudget)
	}
}

// TestStartRemembersTheAttemptBeforeMakingIt keeps a lookup that outlives its
// command from making every later command issue its own request.
func TestStartRemembersTheAttemptBeforeMakingIt(t *testing.T) {
	released := make(chan struct{})
	checker := newChecker(t, &stubFeed{tag: "v0.4.2", block: released}, "0.4.1")
	checker.WaitBudget = 20 * time.Millisecond

	lookup := checker.Start(context.Background())
	awaitLookup(t, lookup, released)
	lookup.Notice()

	remembered, present := readState(checker.CachePath)
	if !present {
		t.Fatal("the attempt was not remembered before it was made")
	}
	if remembered.CheckedAt.IsZero() {
		t.Fatal("the remembered attempt carries no timestamp")
	}
}

// TestStartKeepsAKnownReleaseAcrossAFailedRefresh keeps a network blip from
// forgetting a release the cache already knew about.
func TestStartKeepsAKnownReleaseAcrossAFailedRefresh(t *testing.T) {
	checker := newChecker(t, &stubFeed{err: errors.New("no network")}, "0.4.1")
	checker.MaxAge = time.Hour
	writeState(checker.CachePath, state{
		CheckedAt:     time.Now().Add(-2 * time.Hour),
		LatestVersion: "v0.4.2",
	})

	notice := checker.Start(context.Background()).Notice()
	if !strings.Contains(notice, "0.4.2 is available") {
		t.Fatalf("notice = %q, want the previously known 0.4.2", notice)
	}
	remembered, present := readState(checker.CachePath)
	if !present || remembered.LatestVersion != "v0.4.2" {
		t.Fatalf("remembered %+v, want v0.4.2 carried forward", remembered)
	}
}

func TestStartIgnoresAnOlderFeedAnswerThanTheCache(t *testing.T) {
	checker := newChecker(t, &stubFeed{tag: "v0.4.1"}, "0.4.0")
	checker.MaxAge = time.Hour
	writeState(checker.CachePath, state{
		CheckedAt:     time.Now().Add(-2 * time.Hour),
		LatestVersion: "v0.4.2",
	})

	notice := checker.Start(context.Background()).Notice()
	if !strings.Contains(notice, "0.4.2 is available") {
		t.Fatalf("notice = %q, want the newer of the two known versions", notice)
	}
}

func TestNoticeOnAnAbsentLookup(t *testing.T) {
	var lookup *Lookup
	if notice := lookup.Notice(); notice != "" {
		t.Fatalf("notice = %q, want none", notice)
	}
	var checker *Checker
	if got := checker.Start(context.Background()); got != nil {
		t.Fatal("a nil checker started a lookup")
	}
}

func TestReadStateRejectsUnusableRecords(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		contents string
	}{
		{name: "truncated", contents: `{"checked_at":"2026-0`},
		{name: "not json", contents: "0.4.2"},
		{name: "empty", contents: ""},
		{name: "missing timestamp", contents: `{"latest_version":"v0.4.2"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(testtemp.Dir(t), "update-check.json")
			if err := os.WriteFile(path, []byte(testCase.contents), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, present := readState(path); present {
				t.Fatal("an unusable record was accepted")
			}
		})
	}
}

// TestReadStateAcceptsARecordedAttempt covers the record written before a
// lookup is made: it carries a timestamp and no usable version, and it has to
// read back as a valid attempt so the next command honors it.
func TestReadStateAcceptsARecordedAttempt(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		contents string
	}{
		{name: "no version recorded", contents: `{"checked_at":"2026-01-01T00:00:00Z"}`},
		{name: "unusable version recorded", contents: `{"checked_at":"2026-01-01T00:00:00Z","latest_version":"latest"}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(testtemp.Dir(t), "update-check.json")
			if err := os.WriteFile(path, []byte(testCase.contents), 0o644); err != nil {
				t.Fatal(err)
			}
			loaded, present := readState(path)
			if !present {
				t.Fatal("a recorded attempt was rejected")
			}
			if loaded.LatestVersion != "" {
				t.Fatalf("LatestVersion = %q, want it normalized to empty", loaded.LatestVersion)
			}
		})
	}
}

func TestStateRoundTrips(t *testing.T) {
	path := filepath.Join(testtemp.Dir(t), "nested", "update-check.json")
	written := state{CheckedAt: time.Now().UTC().Truncate(time.Second), LatestVersion: "v0.4.2"}
	writeState(path, written)

	loaded, present := readState(path)
	if !present {
		t.Fatal("the record did not round-trip")
	}
	if !loaded.CheckedAt.Equal(written.CheckedAt) || loaded.LatestVersion != written.LatestVersion {
		t.Fatalf("loaded %+v, want %+v", loaded, written)
	}
	// Nothing but the record itself may be left behind in the cache directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("cache directory holds %v, want only the record", names)
	}
}

// TestCheckerToleratesAnUnusableCache covers a read-only or unresolvable cache
// location: the check degrades to one lookup per command and never fails.
func TestCheckerToleratesAnUnusableCache(t *testing.T) {
	for _, path := range []string{"", filepath.Join(testtemp.Dir(t), "unwritable", "update-check.json")} {
		if path != "" {
			parent := filepath.Dir(path)
			if err := os.MkdirAll(parent, 0o500); err != nil {
				t.Fatal(err)
			}
		}
		checker := newChecker(t, &stubFeed{tag: "v0.4.2"}, "0.4.1")
		checker.CachePath = path
		if notice := checker.Start(context.Background()).Notice(); !strings.Contains(notice, "0.4.2") {
			t.Fatalf("notice = %q for cache path %q, want the release reported anyway", notice, path)
		}
	}
}

func TestCachePathHonorsItsOverride(t *testing.T) {
	t.Setenv(EnvCachePath, filepath.Join(testtemp.Dir(t), "elsewhere.json"))
	path, err := CachePath()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "elsewhere.json" {
		t.Fatalf("CachePath = %q, want the override", path)
	}

	t.Setenv(EnvCachePath, "   ")
	if _, err := CachePath(); err == nil {
		t.Fatal("an empty override was accepted")
	}
}

func TestCachePathDefaultsBesideTheOtherCaches(t *testing.T) {
	// t.Setenv registers the restore, including restoring the variable to unset;
	// the removal below is what this test actually needs.
	t.Setenv(EnvCachePath, "placeholder")
	if err := os.Unsetenv(EnvCachePath); err != nil {
		t.Fatal(err)
	}
	path, err := CachePath()
	if err != nil {
		t.Skipf("no user cache directory available: %v", err)
	}
	if filepath.Base(path) != "update-check.json" || filepath.Base(filepath.Dir(path)) != "grafo" {
		t.Fatalf("CachePath = %q, want grafo/update-check.json under the user cache directory", path)
	}
}

func TestDisabledReadsTheEnvironment(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		environment map[string]string
		want        bool
	}{
		{name: "nothing set", want: false},
		{name: "opted out", environment: map[string]string{EnvDisable: "1"}, want: true},
		{name: "opted out with any value", environment: map[string]string{EnvDisable: "yes"}, want: true},
		{name: "an empty value is not an opt-out", environment: map[string]string{EnvDisable: ""}, want: false},
		{name: "an empty CI is not continuous integration", environment: map[string]string{"CI": ""}, want: false},
		{name: "explicitly opted in", environment: map[string]string{EnvDisable: "0"}, want: false},
		{name: "continuous integration", environment: map[string]string{"CI": "true"}, want: true},
		{name: "continuous integration disclaimed", environment: map[string]string{"CI": "false"}, want: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			lookupEnv := func(name string) (string, bool) {
				value, present := testCase.environment[name]
				return value, present
			}
			if got := Disabled(lookupEnv); got != testCase.want {
				t.Fatalf("Disabled = %t, want %t", got, testCase.want)
			}
		})
	}
}

func TestLatestReleaseReportsItsFailures(t *testing.T) {
	checker := newChecker(t, &stubFeed{err: errors.New("no network")}, "0.4.1")
	if _, err := checker.LatestRelease(context.Background()); err == nil {
		t.Fatal("LatestRelease hid a failed lookup")
	}
}

// TestLatestReleaseBypassesTheCache covers the explicit check: it must report
// what the feed says now, not what was remembered.
func TestLatestReleaseBypassesTheCache(t *testing.T) {
	feed := &stubFeed{tag: "v0.4.3"}
	checker := newChecker(t, feed, "0.4.1")
	writeState(checker.CachePath, state{CheckedAt: time.Now(), LatestVersion: "v0.4.2"})

	latest, err := checker.LatestRelease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if latest != "v0.4.3" {
		t.Fatalf("LatestRelease = %q, want the live v0.4.3", latest)
	}
}

func TestNewCheckerResolvesItsDefaults(t *testing.T) {
	t.Setenv(EnvCachePath, filepath.Join(testtemp.Dir(t), "update-check.json"))
	checker := NewChecker("0.4.1")
	if checker.CurrentVersion != "0.4.1" {
		t.Fatalf("CurrentVersion = %q, want 0.4.1", checker.CurrentVersion)
	}
	if checker.Feed == nil {
		t.Fatal("NewChecker left the feed unset")
	}
	if checker.CachePath == "" {
		t.Fatal("NewChecker left the cache path unset")
	}
	if checker.Method == "" {
		t.Fatal("NewChecker left the install method unset")
	}
}

// TestStateIsNotKeyedByTheRunningVersion keeps a downgrade from hiding a
// release the cache already knows about.
func TestStateIsNotKeyedByTheRunningVersion(t *testing.T) {
	path := filepath.Join(testtemp.Dir(t), "update-check.json")
	writeState(path, state{CheckedAt: time.Now(), LatestVersion: "v0.4.2"})
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(contents, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 {
		t.Fatalf("record holds %v, want only the timestamp and the version", decoded)
	}
}
