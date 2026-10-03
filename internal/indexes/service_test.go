package indexes

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cafecito-games/grafo/internal/graph"
	"github.com/cafecito-games/grafo/internal/indexer"
	"github.com/cafecito-games/grafo/internal/service"
	"github.com/cafecito-games/grafo/internal/storage/sqlite"
	"github.com/cafecito-games/grafo/internal/testtemp"
)

func TestListIsDeterministicAndAccountsForEveryBranchFile(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	current := seedIndex(t, project.IndexPath, project, project.Branch, now.Add(-time.Hour))
	recentPath := filepath.Join(filepath.Dir(project.IndexPath), "recent.sqlite")
	recent := seedIndex(t, recentPath, project, "recent", now.Add(-24*time.Hour))
	oldPath := filepath.Join(filepath.Dir(project.IndexPath), "old.sqlite")
	old := seedIndex(t, oldPath, project, "old", now.Add(-48*time.Hour))
	defer closeRepositories(t, current, recent, old)

	inventory, err := List(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Indexes) != 3 {
		t.Fatalf("indexes = %#v", inventory.Indexes)
	}
	if got := []string{inventory.Indexes[0].Branch, inventory.Indexes[1].Branch, inventory.Indexes[2].Branch}; !reflect.DeepEqual(got, []string{project.Branch, "recent", "old"}) {
		t.Fatalf("order = %q", got)
	}
	var totals Sizes
	sawSidecar := false
	for _, candidate := range inventory.Indexes {
		if candidate.Compatibility != CompatibilityCompatible || !candidate.Verified {
			t.Fatalf("candidate was not verified: %#v", candidate)
		}
		if candidate.Metrics == nil || candidate.Metrics.PageSize <= 0 || candidate.Metrics.PageCount <= 0 {
			t.Fatalf("candidate metrics = %#v", candidate.Metrics)
		}
		if candidate.Filename != filepath.Base(candidate.Path) || !filepath.IsAbs(candidate.Path) {
			t.Fatalf("candidate paths = %#v", candidate)
		}
		wantSizes := Sizes{
			Database: fileSize(t, candidate.Path),
			WAL:      fileSize(t, candidate.Path+"-wal"),
			SHM:      fileSize(t, candidate.Path+"-shm"),
		}
		wantSizes.Total = wantSizes.Database + wantSizes.WAL + wantSizes.SHM
		if candidate.Sizes != wantSizes {
			t.Fatalf("sizes for %s = %#v, want physical %#v", candidate.Filename, candidate.Sizes, wantSizes)
		}
		sawSidecar = sawSidecar || candidate.Sizes.WAL > 0 || candidate.Sizes.SHM > 0
		totals = totals.Add(candidate.Sizes)
	}
	if inventory.Totals != totals || totals.Total == 0 || totals.Total != totals.Database+totals.WAL+totals.SHM || !sawSidecar {
		t.Fatalf("totals = %#v, summed %#v", inventory.Totals, totals)
	}
	for _, candidate := range inventory.Indexes {
		if candidate.Sizes.Total*2 >= inventory.Totals.Total {
			t.Fatalf("three branch fixtures did not demonstrate additive disk growth: candidate=%#v totals=%#v", candidate.Sizes, inventory.Totals)
		}
	}
}

func TestRecommendCompactionRequiresBothThresholds(t *testing.T) {
	const mib = int64(1 << 20)
	for _, test := range []struct {
		name        string
		primary     int64
		reclaimable int64
		want        bool
	}{
		{name: "both exact", primary: 1280 * mib, reclaimable: 256 * mib, want: true},
		{name: "bytes below", primary: 1280 * mib, reclaimable: 256*mib - 1},
		{name: "percent below", primary: 1280*mib + 1, reclaimable: 256 * mib},
		{name: "larger", primary: 2 << 30, reclaimable: 512 * mib, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			metrics := sqlite.StorageMetrics{PageSize: sqlite.TargetPageSize, ReclaimableBytes: test.reclaimable}
			if got := recommendCompaction(test.primary, metrics); got != test.want {
				t.Fatalf("recommendCompaction(%d, %d) = %t, want %t", test.primary, test.reclaimable, got, test.want)
			}
		})
	}
}

func TestCompactionReclaimedReportsAPageSizeUpgradeAsMeasured(t *testing.T) {
	before := Sizes{Database: 2800, WAL: 16, SHM: 4, Total: 2820}
	after := Sizes{Database: 2130, WAL: 0, SHM: 4, Total: 2134}
	emptyFreelist := sqlite.StorageMetrics{PageSize: legacyPageSize}
	for _, test := range []struct {
		name       string
		compaction sqlite.CompactionResult
		want       Sizes
	}{
		{
			name: "nothing to reclaim and no new page size",
			compaction: sqlite.CompactionResult{
				Before: sqlite.StorageMetrics{PageSize: sqlite.TargetPageSize},
				After:  sqlite.StorageMetrics{PageSize: sqlite.TargetPageSize},
			},
			want: Sizes{},
		},
		{
			name: "page size upgraded with an empty freelist",
			compaction: sqlite.CompactionResult{
				Before: emptyFreelist,
				After:  sqlite.StorageMetrics{PageSize: sqlite.TargetPageSize},
			},
			want: Sizes{Database: 670, WAL: 16, SHM: 0, Total: 686},
		},
		{
			name: "freelist released",
			compaction: sqlite.CompactionResult{
				Before: sqlite.StorageMetrics{PageSize: sqlite.TargetPageSize, ReclaimableBytes: 1},
				After:  sqlite.StorageMetrics{PageSize: sqlite.TargetPageSize},
			},
			want: Sizes{Database: 670, WAL: 16, SHM: 0, Total: 686},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := compactionReclaimed(before, after, test.compaction); got != test.want {
				t.Fatalf("compactionReclaimed = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestRecommendCompactionForALegacyPageSizeRegardlessOfFreelist(t *testing.T) {
	for _, test := range []struct {
		name     string
		pageSize int64
		want     bool
	}{
		{name: "legacy default", pageSize: legacyPageSize, want: true},
		{name: "one below target", pageSize: sqlite.TargetPageSize / 2, want: true},
		{name: "target", pageSize: sqlite.TargetPageSize},
		{name: "above target", pageSize: 2 * sqlite.TargetPageSize},
		{name: "unknown", pageSize: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			metrics := sqlite.StorageMetrics{PageSize: test.pageSize}
			if got := recommendCompaction(1<<30, metrics); got != test.want {
				t.Fatalf("recommendCompaction(page_size=%d) = %t, want %t", test.pageSize, got, test.want)
			}
		})
	}
}

func TestCompactDryRunIsReadOnlyAndRealRunReportsCurrentIndex(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	repository := seedIndex(t, project.IndexPath, project, project.Branch, time.Now().UTC())
	closeRepositories(t, repository)
	before := snapshotFiles(t, filepath.Dir(project.IndexPath))
	beforeInfo, err := os.Stat(project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}

	manager := newManager()
	manager.lock = func(string, time.Duration) (service.Unlock, error) {
		t.Fatal("dry run acquired a writer lock")
		return nil, nil
	}
	dry, err := manager.compact(ctx, root, CompactPolicy{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.Path != project.IndexPath || dry.After != nil || dry.Before.Metrics.PageCount <= 0 || dry.ExpectedUpperBoundBytes != dry.Before.Metrics.LiveAllocatedBytes {
		t.Fatalf("dry report = %#v", dry)
	}
	if after := snapshotFiles(t, filepath.Dir(project.IndexPath)); !reflect.DeepEqual(before, after) {
		t.Fatalf("dry run changed files: before=%#v after=%#v", before, after)
	}
	afterInfo, err := os.Stat(project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("dry run changed primary timestamp: before=%s after=%s", beforeInfo.ModTime(), afterInfo.ModTime())
	}

	real, err := Compact(ctx, root, CompactPolicy{Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if real.DryRun || real.After == nil || real.Path != project.IndexPath || real.After.Metrics.PageCount <= 0 {
		t.Fatalf("real report = %#v", real)
	}
	if real.Before.Metrics.ReclaimableBytes == 0 && real.Reclaimed.Total != 0 {
		t.Fatalf("empty-freelist compaction claimed reclaimed bytes: %#v", real.Reclaimed)
	}
	if _, err := os.Stat(project.IndexPath + ".lock"); err != nil {
		t.Fatalf("compaction removed lock anchor: %v", err)
	}
}

func TestCompactUpgradesALegacyPageSizeAndKeepsTheBranchIdentity(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	repository := seedIndex(t, project.IndexPath, project, project.Branch, time.Now().UTC())
	closeRepositories(t, repository)
	rewriteAtLegacyPageSize(t, project.IndexPath)
	beforeInventory, err := List(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeInventory.Indexes) != 1 || !beforeInventory.Indexes[0].CompactRecommended {
		t.Fatalf("legacy page size was not flagged for compaction: %#v", beforeInventory.Indexes)
	}

	report, err := Compact(ctx, root, CompactPolicy{Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.After == nil {
		t.Fatalf("report = %#v", report)
	}
	if report.Before.Metrics.PageSize != legacyPageSize || report.After.Metrics.PageSize != sqlite.TargetPageSize {
		t.Fatalf("page size went from %d to %d, want %d to %d",
			report.Before.Metrics.PageSize, report.After.Metrics.PageSize, legacyPageSize, sqlite.TargetPageSize)
	}
	onDisk, err := os.Stat(project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	if report.After.Sizes.Database != onDisk.Size() {
		t.Fatalf("reported database size %d, on disk %d", report.After.Sizes.Database, onDisk.Size())
	}
	if _, err := os.Stat(project.IndexPath + ".lock"); err != nil {
		t.Fatalf("compaction removed lock anchor: %v", err)
	}
	afterInventory, err := List(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterInventory.Indexes) != 1 {
		t.Fatalf("inventory after upgrade = %#v", afterInventory.Indexes)
	}
	upgraded := afterInventory.Indexes[0]
	legacy := beforeInventory.Indexes[0]
	if upgraded.Compatibility != CompatibilityCompatible || !upgraded.Verified || upgraded.CompactRecommended {
		t.Fatalf("upgraded index = %#v", upgraded)
	}
	identity := func(index Index) [5]string {
		return [5]string{index.Filename, index.Root, index.RepositoryID, index.Branch, index.Commit}
	}
	if identity(upgraded) != identity(legacy) || upgraded.IndexedAt != legacy.IndexedAt {
		t.Fatalf("upgrade changed branch identity: before=%#v after=%#v", legacy, upgraded)
	}
}

const legacyPageSize = 4096

// rewriteAtLegacyPageSize leaves an index the way a version before the 16 KiB
// default would have: in WAL mode at SQLite's old default page size.
func rewriteAtLegacyPageSize(t *testing.T, path string) {
	t.Helper()
	ctx := context.Background()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	var mode string
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "PRAGMA page_size=4096"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "VACUUM"); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	var pageSize int64
	if err := database.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if pageSize != legacyPageSize || !strings.EqualFold(mode, "wal") {
		t.Fatalf("legacy fixture is page_size=%d journal_mode=%q", pageSize, mode)
	}
}

func TestCompactDryRunAndRealRunBothRefuseNonWALWithoutMutation(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	repository := seedIndex(t, project.IndexPath, project, project.Branch, time.Now().UTC())
	closeRepositories(t, repository)

	database, err := sql.Open("sqlite", project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "delete") {
		t.Fatalf("fixture journal mode = %q, want DELETE", mode)
	}
	before := snapshotFiles(t, filepath.Dir(project.IndexPath))

	manager := newManager()
	manager.lock = func(string, time.Duration) (service.Unlock, error) {
		t.Fatal("ineligible compaction acquired a writer lock")
		return nil, nil
	}
	_, dryErr := manager.compact(ctx, root, CompactPolicy{DryRun: true})
	_, realErr := manager.compact(ctx, root, CompactPolicy{Confirm: true})
	if dryErr == nil || realErr == nil || dryErr.Error() != realErr.Error() || !strings.Contains(strings.ToLower(dryErr.Error()), "journal mode") {
		t.Fatalf("non-WAL errors: dry=%v real=%v", dryErr, realErr)
	}
	if after := snapshotFiles(t, filepath.Dir(project.IndexPath)); !reflect.DeepEqual(before, after) {
		t.Fatalf("non-WAL preflight changed files: before=%#v after=%#v", before, after)
	}
	database, err = sql.Open("sqlite", project.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if err := database.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(mode, "delete") {
		t.Fatalf("preflight changed journal mode to %q", mode)
	}
}

func TestCompactValidatesIntentTargetLockAndPendingState(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	manager := newManager()
	discovered := false
	manager.discover = func(context.Context, string) (indexer.Project, error) {
		discovered = true
		return indexer.Project{}, errors.New("unexpected discovery")
	}
	if _, err := manager.compact(ctx, root, CompactPolicy{}); err == nil || discovered {
		t.Fatalf("missing confirmation error=%v discovered=%t", err, discovered)
	}

	project := discoverTestProject(t, root)
	repository := seedIndex(t, project.IndexPath, project, project.Branch, time.Now().UTC())
	if err := repository.ReplaceFile(ctx, graph.FileRecord{Path: "pending.go", Hash: "hash", Language: "go", IndexedAt: graph.NowUTC()}, graph.ParseResult{}); err != nil {
		t.Fatal(err)
	}
	closeRepositories(t, repository)
	if _, err := Compact(ctx, root, CompactPolicy{Confirm: true}); err == nil || !strings.Contains(err.Error(), "reconciliation is pending") {
		t.Fatalf("pending compaction error = %v", err)
	}

	manager = newManager()
	manager.lock = func(string, time.Duration) (service.Unlock, error) { return nil, errors.New("held branch lock") }
	if _, err := manager.compact(ctx, root, CompactPolicy{Confirm: true}); err == nil || !strings.Contains(err.Error(), "held branch lock") {
		t.Fatalf("held lock error = %v", err)
	}

	missingRoot := testtemp.Dir(t)
	if _, err := Compact(ctx, missingRoot, CompactPolicy{DryRun: true}); err == nil || !strings.Contains(err.Error(), "run 'grafo index'") {
		t.Fatalf("missing index error = %v", err)
	}

	symlinkRoot := testtemp.Dir(t)
	symlinkProject := discoverTestProject(t, symlinkRoot)
	if err := os.MkdirAll(filepath.Dir(symlinkProject.IndexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(testtemp.Dir(t), "external.sqlite")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, symlinkProject.IndexPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Compact(ctx, symlinkRoot, CompactPolicy{DryRun: true}); err == nil || !strings.Contains(err.Error(), "non-symlink") {
		t.Fatalf("symlink index error = %v", err)
	}

	for _, test := range []struct {
		name   string
		create func(string, indexer.Project)
	}{
		{name: "corrupt", create: func(path string, _ indexer.Project) {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "incompatible", create: func(path string, project indexer.Project) {
			repository := seedIndex(t, path, project, project.Branch, time.Now().UTC())
			if err := repository.SetMeta(ctx, "semantic_index_version", "0"); err != nil {
				t.Fatal(err)
			}
			closeRepositories(t, repository)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidateRoot := testtemp.Dir(t)
			candidate := discoverTestProject(t, candidateRoot)
			test.create(candidate.IndexPath, candidate)
			if _, err := Compact(ctx, candidateRoot, CompactPolicy{DryRun: true}); err == nil || !strings.Contains(err.Error(), "cannot be compacted") {
				t.Fatalf("%s compaction error = %v", test.name, err)
			}
		})
	}
}

func TestCompactRevalidatesCurrentIndexAfterLock(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	repository := seedIndex(t, project.IndexPath, project, project.Branch, time.Now().UTC())
	otherPath := filepath.Join(filepath.Dir(project.IndexPath), "other.sqlite")
	other := seedIndex(t, otherPath, project, "other", time.Now().UTC())
	closeRepositories(t, repository, other)
	manager := newManager()
	discover := manager.discover
	calls := 0
	manager.discover = func(ctx context.Context, root string) (indexer.Project, error) {
		calls++
		result, err := discover(ctx, root)
		if calls > 1 {
			result.IndexPath = otherPath
		}
		return result, err
	}
	if _, err := manager.compact(ctx, root, CompactPolicy{Confirm: true}); err == nil || !strings.Contains(err.Error(), "current branch index changed") {
		t.Fatalf("changed current index error = %v", err)
	}
	if _, err := os.Stat(project.IndexPath); err != nil {
		t.Fatalf("original index changed: %v", err)
	}
}

func TestListAbsentDirectoryAndUnsafeCandidatesFailClosed(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	inventory, err := List(ctx, root)
	if err != nil || len(inventory.Indexes) != 0 || inventory.Totals.Total != 0 {
		t.Fatalf("absent inventory = %#v, %v", inventory, err)
	}

	if err := os.MkdirAll(filepath.Dir(project.IndexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(testtemp.Dir(t), "outside.sqlite")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(filepath.Dir(project.IndexPath), "linked.sqlite")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(project.IndexPath), "corrupt.sqlite"), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	inventory, err = List(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Indexes) != 2 {
		t.Fatalf("unsafe inventory = %#v", inventory.Indexes)
	}
	states := map[string]Compatibility{}
	for _, candidate := range inventory.Indexes {
		states[candidate.Filename] = candidate.Compatibility
		if candidate.Verified {
			t.Fatalf("unsafe candidate was verified: %#v", candidate)
		}
	}
	if states["linked.sqlite"] != CompatibilityUnverified || states["corrupt.sqlite"] != CompatibilityCorrupt {
		t.Fatalf("compatibility states = %#v", states)
	}
}

func TestListRejectsSymlinkedIndexDirectoryAncestry(t *testing.T) {
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	external := testtemp.Dir(t)
	if err := os.MkdirAll(filepath.Join(external, "indexes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "indexes", "outside.sqlite"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Dir(filepath.Dir(project.IndexPath))); err != nil {
		t.Fatal(err)
	}

	if inventory, err := List(context.Background(), root); err == nil {
		t.Fatalf("symlinked .grafo ancestry was followed: %#v", inventory)
	}
}

func TestPruneIntersectsSelectorsAndDryRunIsNonMutating(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	current := seedIndex(t, project.IndexPath, project, project.Branch, now.Add(-time.Hour))
	recentPath := filepath.Join(filepath.Dir(project.IndexPath), "recent.sqlite")
	recent := seedIndex(t, recentPath, project, "recent", now.Add(-48*time.Hour))
	oldPath := filepath.Join(filepath.Dir(project.IndexPath), "old.sqlite")
	old := seedIndex(t, oldPath, project, "old", now.Add(-72*time.Hour))
	closeRepositories(t, current, recent, old)

	before := snapshotFiles(t, filepath.Dir(project.IndexPath))
	olderThan := 24 * time.Hour
	keep := 2
	dry, err := Prune(ctx, root, Policy{OlderThan: &olderThan, Keep: &keep, DryRun: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedFilenames(dry); !reflect.DeepEqual(got, []string{"old.sqlite"}) {
		t.Fatalf("dry-run selection = %q; report %#v", got, dry.Results)
	}
	afterDryRun := snapshotFiles(t, filepath.Dir(project.IndexPath))
	if !reflect.DeepEqual(before, afterDryRun) {
		t.Fatalf("dry run changed index bytes: before=%#v after=%#v", before, afterDryRun)
	}

	real, err := Prune(ctx, root, Policy{OlderThan: &olderThan, Keep: &keep, Confirm: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if got := selectedFilenames(real); !reflect.DeepEqual(got, []string{"old.sqlite"}) {
		t.Fatalf("real selection = %q; report %#v", got, real.Results)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old index survived: %v", err)
	}
	if _, err := os.Stat(oldPath + ".lock"); err != nil {
		t.Fatalf("successful prune removed lock anchor: %v", err)
	}
	for _, protected := range []string{project.IndexPath, recentPath} {
		if _, err := os.Stat(protected); err != nil {
			t.Fatalf("protected index %s: %v", protected, err)
		}
	}
	if real.Reclaimed.Total == 0 {
		t.Fatal("successful prune did not report reclaimed bytes")
	}
	replay, err := Prune(ctx, root, Policy{OlderThan: &olderThan, Keep: &keep, Confirm: true, Now: now})
	if err != nil || len(selectedFilenames(replay)) != 0 {
		t.Fatalf("idempotent replay = %#v, %v", replay, err)
	}
}

func TestPruneProtectsCurrentAndRejectsUnverifiedFutureAndLockedCandidates(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	current := seedIndex(t, project.IndexPath, project, project.Branch, now.Add(-72*time.Hour))
	lockedPath := filepath.Join(filepath.Dir(project.IndexPath), "locked.sqlite")
	locked := seedIndex(t, lockedPath, project, "locked", now.Add(-72*time.Hour))
	futurePath := filepath.Join(filepath.Dir(project.IndexPath), "future.sqlite")
	future := seedIndex(t, futurePath, project, "future", now.Add(time.Hour))
	mismatchPath := filepath.Join(filepath.Dir(project.IndexPath), "mismatch.sqlite")
	mismatch := seedIndexWithIdentity(t, mismatchPath, project, "mismatch", now.Add(-72*time.Hour), "other-repository")
	incompatiblePath := filepath.Join(filepath.Dir(project.IndexPath), "incompatible.sqlite")
	incompatible := seedIndex(t, incompatiblePath, project, "incompatible", now.Add(-72*time.Hour))
	if err := incompatible.SetMeta(ctx, "semantic_index_version", "0"); err != nil {
		t.Fatal(err)
	}
	metadataFreePath := filepath.Join(filepath.Dir(project.IndexPath), "metadata-free.sqlite")
	metadataFree, err := sqlite.Open(ctx, metadataFreePath)
	if err != nil {
		t.Fatal(err)
	}
	closeRepositories(t, current, locked, future, mismatch, incompatible, metadataFree)
	malformedTimePath := filepath.Join(filepath.Dir(project.IndexPath), "malformed-time.sqlite")
	malformedTime := seedIndex(t, malformedTimePath, project, "malformed-time", now.Add(-72*time.Hour))
	if err := malformedTime.SetMeta(ctx, "indexed_at", "2026-09-25T12:00:00+00:00"); err != nil {
		t.Fatal(err)
	}
	sidecarSymlinkPath := filepath.Join(filepath.Dir(project.IndexPath), "sidecar-symlink.sqlite")
	sidecarSymlink := seedIndex(t, sidecarSymlinkPath, project, "sidecar-symlink", now.Add(-72*time.Hour))
	closeRepositories(t, malformedTime, sidecarSymlink)
	corruptPath := filepath.Join(filepath.Dir(project.IndexPath), "corrupt.sqlite")
	if err := os.WriteFile(corruptPath, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(testtemp.Dir(t), "external.sqlite")
	if err := os.WriteFile(external, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(filepath.Dir(project.IndexPath), "symlink.sqlite")
	if err := os.Symlink(external, symlinkPath); err != nil {
		t.Fatal(err)
	}
	unsafeSidecar := filepath.Join(testtemp.Dir(t), "outside-wal")
	if err := os.WriteFile(unsafeSidecar, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(unsafeSidecar, sidecarSymlinkPath+"-wal"); err != nil {
		t.Fatal(err)
	}

	unlock, acquired, err := service.TryIndexLock(lockedPath)
	if err != nil || !acquired {
		t.Fatalf("lock fixture = %t, %v", acquired, err)
	}
	defer func() { _ = unlock() }()
	zero := time.Duration(0)
	keepNone := 0
	report, err := Prune(ctx, root, Policy{OlderThan: &zero, Keep: &keepNone, Confirm: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]Status{}
	for _, result := range report.Results {
		statuses[result.Index.Filename] = result.Status
	}
	if statuses[filepath.Base(project.IndexPath)] != StatusProtected || statuses["locked.sqlite"] != StatusLocked {
		t.Fatalf("protected/locked statuses = %#v", statuses)
	}
	if statuses["future.sqlite"] != StatusProtected {
		t.Fatalf("future.sqlite status = %q; all %#v", statuses["future.sqlite"], statuses)
	}
	for _, filename := range []string{"mismatch.sqlite", "incompatible.sqlite", "metadata-free.sqlite", "malformed-time.sqlite", "sidecar-symlink.sqlite", "corrupt.sqlite", "symlink.sqlite"} {
		if statuses[filename] != StatusIneligible {
			t.Fatalf("%s status = %q; all %#v", filename, statuses[filename], statuses)
		}
	}
	for _, path := range []string{project.IndexPath, lockedPath, futurePath, mismatchPath, incompatiblePath, metadataFreePath, malformedTimePath, sidecarSymlinkPath, corruptPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("protected path %s: %v", path, err)
		}
	}
	if _, err := os.Lstat(symlinkPath); err != nil {
		t.Fatalf("protected symlink missing: %v", err)
	}
	if _, err := os.Lstat(sidecarSymlinkPath + "-wal"); err != nil {
		t.Fatalf("protected sidecar symlink missing: %v", err)
	}
	if _, err := os.Stat(lockedPath + ".lock"); err != nil {
		t.Fatalf("lock anchor missing: %v", err)
	}
}

func TestRetainedIndexesCountsCurrentTowardKeepLimit(t *testing.T) {
	keep := 1
	current := Index{Path: "current.sqlite", Current: true, Verified: true, indexedTime: time.Unix(1, 0)}
	newerBranch := Index{Path: "newer.sqlite", Verified: true, indexedTime: time.Unix(2, 0)}
	retained := retainedIndexes([]Index{current, newerBranch}, &keep)
	if !retained[current.Path] || retained[newerBranch.Path] {
		t.Fatalf("retained = %#v; current must consume the sole keep slot", retained)
	}
}

func TestPruneRevalidatesCurrentIndexUnderCandidateLock(t *testing.T) {
	ctx := context.Background()
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	current := seedIndex(t, project.IndexPath, project, project.Branch, now.Add(-time.Hour))
	oldPath := filepath.Join(filepath.Dir(project.IndexPath), "old.sqlite")
	old := seedIndex(t, oldPath, project, "old", now.Add(-72*time.Hour))
	closeRepositories(t, current, old)

	manager := newManager()
	discover := manager.discover
	discoveries := 0
	manager.discover = func(ctx context.Context, requestedRoot string) (indexer.Project, error) {
		discoveries++
		if discoveries == 1 {
			return discover(ctx, requestedRoot)
		}
		switched := project
		switched.Branch = "old"
		switched.IndexPath = oldPath
		return switched, nil
	}
	keep := 0
	report, err := manager.prune(ctx, root, Policy{Keep: &keep, Confirm: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	result := resultFor(report, "old.sqlite")
	if result == nil || result.Status != StatusProtected || result.Selected {
		t.Fatalf("newly current result = %#v", result)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("newly current index was deleted: %v", err)
	}
}

func TestPruneValidatesBeforeWorkAndReportsPartialFailureAndCancellation(t *testing.T) {
	root := testtemp.Dir(t)
	project := discoverTestProject(t, root)
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if _, err := Prune(context.Background(), root, Policy{Confirm: true, Now: now}); err == nil {
		t.Fatal("prune accepted no retention selector")
	}
	negativeDuration := -time.Second
	if _, err := Prune(context.Background(), root, Policy{OlderThan: &negativeDuration, Confirm: true, Now: now}); err == nil {
		t.Fatal("prune accepted a negative duration")
	}
	negativeKeep := -1
	if _, err := Prune(context.Background(), root, Policy{Keep: &negativeKeep, Confirm: true, Now: now}); err == nil {
		t.Fatal("prune accepted a negative keep count")
	}
	keep := 0
	if _, err := Prune(context.Background(), root, Policy{Keep: &keep, Now: now}); err == nil {
		t.Fatal("mutating prune did not require confirmation")
	}

	firstPath := filepath.Join(filepath.Dir(project.IndexPath), "a-old.sqlite")
	secondPath := filepath.Join(filepath.Dir(project.IndexPath), "b-old.sqlite")
	first := seedIndex(t, firstPath, project, "a-old", now.Add(-72*time.Hour))
	second := seedIndex(t, secondPath, project, "b-old", now.Add(-48*time.Hour))
	closeRepositories(t, first, second)
	manager := newManager()
	originalRemove := manager.remove
	manager.remove = func(path string) error {
		if path == firstPath {
			return errors.New("injected primary removal failure")
		}
		return originalRemove(path)
	}
	report, err := manager.prune(context.Background(), root, Policy{Keep: &keep, Confirm: true, Now: now})
	if err == nil || !strings.Contains(err.Error(), "injected primary removal failure") {
		t.Fatalf("partial failure error = %v; report %#v", err, report)
	}
	if report.Reclaimed.Total == 0 {
		t.Fatal("successful sibling deletion was not reported")
	}
	if failed := resultFor(report, "a-old.sqlite"); failed == nil || failed.Status != StatusFailed || failed.Reclaimed.Total != 0 {
		t.Fatalf("failed primary removal claimed bytes: %#v", failed)
	}
	if _, err := os.Stat(firstPath); err != nil {
		t.Fatalf("failed primary was removed: %v", err)
	}
	if _, err := os.Stat(secondPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("independent sibling was not deleted: %v", err)
	}

	cancelRoot := testtemp.Dir(t)
	cancelProject := discoverTestProject(t, cancelRoot)
	thirdPath := filepath.Join(filepath.Dir(cancelProject.IndexPath), "c-old.sqlite")
	fourthPath := filepath.Join(filepath.Dir(cancelProject.IndexPath), "d-old.sqlite")
	third := seedIndex(t, thirdPath, cancelProject, "c-old", now.Add(-96*time.Hour))
	fourth := seedIndex(t, fourthPath, cancelProject, "d-old", now.Add(-120*time.Hour))
	closeRepositories(t, third, fourth)
	cancelCtx, cancel := context.WithCancel(context.Background())
	manager = newManager()
	originalRemove = manager.remove
	manager.remove = func(path string) error {
		err := originalRemove(path)
		if path == thirdPath && err == nil {
			cancel()
		}
		return err
	}
	report, err = manager.prune(cancelCtx, cancelRoot, Policy{Keep: &keep, Confirm: true, Now: now})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, statErr := os.Stat(thirdPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("completed deletion not retained: %v", statErr)
	}
	if _, statErr := os.Stat(fourthPath); statErr != nil {
		t.Fatalf("unselected candidate changed after cancellation: %v", statErr)
	}
	if len(selectedFilenames(report)) != 1 {
		t.Fatalf("cancellation report = %#v", report.Results)
	}
}

func TestPruneCheckpointAndSidecarFailuresAreTruthful(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	keep := 0

	t.Run("checkpoint failure leaves index files untouched", func(t *testing.T) {
		root := testtemp.Dir(t)
		project := discoverTestProject(t, root)
		path := filepath.Join(filepath.Dir(project.IndexPath), "old.sqlite")
		repository := seedIndex(t, path, project, "old", now.Add(-72*time.Hour))
		closeRepositories(t, repository)
		before := snapshotIndexFiles(t, path)
		manager := newManager()
		manager.checkpoint = func(context.Context, string, sqlite.IndexMetadata) error {
			return errors.New("injected checkpoint failure")
		}
		report, err := manager.prune(context.Background(), root, Policy{Keep: &keep, Confirm: true, Now: now})
		if err == nil || !strings.Contains(err.Error(), "injected checkpoint failure") {
			t.Fatalf("checkpoint failure = %v; report %#v", err, report.Results)
		}
		if after := snapshotIndexFiles(t, path); !reflect.DeepEqual(before, after) {
			t.Fatalf("checkpoint failure changed index files: before=%#v after=%#v", before, after)
		}
	})

	t.Run("disappearing sidecar is already absent", func(t *testing.T) {
		root := testtemp.Dir(t)
		project := discoverTestProject(t, root)
		path := filepath.Join(filepath.Dir(project.IndexPath), "old.sqlite")
		repository := seedIndex(t, path, project, "old", now.Add(-72*time.Hour))
		closeRepositories(t, repository)
		manager := newManager()
		checkpoint := manager.checkpoint
		manager.checkpoint = func(ctx context.Context, candidate string, metadata sqlite.IndexMetadata) error {
			if err := checkpoint(ctx, candidate, metadata); err != nil {
				return err
			}
			return os.WriteFile(candidate+"-wal", []byte("sidecar"), 0o600)
		}
		remove := manager.remove
		manager.remove = func(target string) error {
			if target == path+"-wal" {
				if err := remove(target); err != nil {
					return err
				}
				return os.ErrNotExist
			}
			return remove(target)
		}
		report, err := manager.prune(context.Background(), root, Policy{Keep: &keep, Confirm: true, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if result := resultFor(report, "old.sqlite"); result == nil || result.Status != StatusDeleted {
			t.Fatalf("disappearing sidecar result = %#v", result)
		}
	})

	t.Run("sidecar failure reports partial reclaimed bytes", func(t *testing.T) {
		root := testtemp.Dir(t)
		project := discoverTestProject(t, root)
		path := filepath.Join(filepath.Dir(project.IndexPath), "old.sqlite")
		repository := seedIndex(t, path, project, "old", now.Add(-72*time.Hour))
		closeRepositories(t, repository)
		manager := newManager()
		checkpoint := manager.checkpoint
		manager.checkpoint = func(ctx context.Context, candidate string, metadata sqlite.IndexMetadata) error {
			if err := checkpoint(ctx, candidate, metadata); err != nil {
				return err
			}
			return os.WriteFile(candidate+"-wal", []byte("sidecar"), 0o600)
		}
		remove := manager.remove
		manager.remove = func(target string) error {
			if target == path+"-wal" {
				return errors.New("injected sidecar failure")
			}
			return remove(target)
		}
		report, err := manager.prune(context.Background(), root, Policy{Keep: &keep, Confirm: true, Now: now})
		if err == nil || !strings.Contains(err.Error(), "injected sidecar failure") {
			t.Fatalf("sidecar failure = %v; report %#v", err, report.Results)
		}
		result := resultFor(report, "old.sqlite")
		if result == nil || result.Status != StatusFailed || result.Reclaimed.Database == 0 || result.Reclaimed.WAL != 0 {
			t.Fatalf("partial reclaimed bytes = %#v", result)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("successfully removed primary reappeared: %v", err)
		}
		if _, err := os.Stat(path + "-wal"); err != nil {
			t.Fatalf("failed sidecar removal was not preserved: %v", err)
		}
	})
}

func discoverTestProject(t *testing.T, root string) indexer.Project {
	t.Helper()
	project, err := indexer.DiscoverProject(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func seedIndex(t *testing.T, path string, project indexer.Project, branch string, indexedAt time.Time) *sqlite.Repository {
	t.Helper()
	return seedIndexWithIdentity(t, path, project, branch, indexedAt, project.ID)
}

func seedIndexWithIdentity(t *testing.T, path string, project indexer.Project, branch string, indexedAt time.Time, repositoryID string) *sqlite.Repository {
	t.Helper()
	repository, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"root": project.Root, "repository_id": repositoryID, "branch": branch,
		"commit": "commit-" + branch, "indexed_at": indexedAt.UTC().Format(time.RFC3339Nano),
		"semantic_index_version": indexer.SemanticIndexVersion,
		"padding":                strings.Repeat("x", 128*1024),
	} {
		if err := repository.SetMeta(context.Background(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	return repository
}

func closeRepositories(t *testing.T, repositories ...*sqlite.Repository) {
	t.Helper()
	for _, repository := range repositories {
		if err := repository.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func selectedFilenames(report PruneReport) []string {
	var result []string
	for _, item := range report.Results {
		if item.Selected {
			result = append(result, item.Index.Filename)
		}
	}
	return result
}

func resultFor(report PruneReport, filename string) *Result {
	for index := range report.Results {
		if report.Results[index].Index.Filename == filename {
			return &report.Results[index]
		}
	}
	return nil
}

func snapshotFiles(t *testing.T, directory string) map[string][32]byte {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string][32]byte)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = sha256.Sum256(content)
	}
	return result
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func snapshotIndexFiles(t *testing.T, primary string) map[string][32]byte {
	t.Helper()
	result := map[string][32]byte{}
	for _, path := range []string{primary, primary + "-wal", primary + "-shm"} {
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		result[filepath.Base(path)] = sha256.Sum256(content)
	}
	return result
}
