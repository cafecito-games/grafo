package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPartitionAssignsEveryPackageToExactlyOneShard(t *testing.T) {
	shards := []shard{
		{Name: "a", Packages: []string{"example.com/m/b"}},
		{Name: "rest"},
	}
	all := []string{"example.com/m/a", "example.com/m/b", "example.com/m/c"}

	got, err := partition(shards, all)
	if err != nil {
		t.Fatalf("partition: %v", err)
	}

	want := map[string][]string{
		"a":    {"example.com/m/b"},
		"rest": {"example.com/m/a", "example.com/m/c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("partition = %v, want %v", got, want)
	}
}

func TestPartitionPutsNewPackagesInTheComplementShard(t *testing.T) {
	shards := []shard{
		{Name: "a", Packages: []string{"example.com/m/a"}},
		{Name: "rest"},
	}

	got, err := partition(shards, []string{"example.com/m/a", "example.com/m/brand-new"})
	if err != nil {
		t.Fatalf("partition: %v", err)
	}
	if want := []string{"example.com/m/brand-new"}; !reflect.DeepEqual(got["rest"], want) {
		t.Fatalf("complement shard = %v, want %v", got["rest"], want)
	}
}

func TestPartitionRejectsShardsThatCannotCoverEveryPackage(t *testing.T) {
	cases := map[string]struct {
		shards  []shard
		all     []string
		wantErr string
	}{
		"no complement shard": {
			shards:  []shard{{Name: "a", Packages: []string{"example.com/m/a"}}},
			all:     []string{"example.com/m/a", "example.com/m/b"},
			wantErr: "exactly one shard must claim no packages",
		},
		"two complement shards": {
			shards:  []shard{{Name: "a"}, {Name: "b"}},
			all:     []string{"example.com/m/a"},
			wantErr: "exactly one shard must claim no packages",
		},
		"named package no longer exists": {
			shards:  []shard{{Name: "a", Packages: []string{"example.com/m/renamed"}}, {Name: "rest"}},
			all:     []string{"example.com/m/a"},
			wantErr: "example.com/m/renamed",
		},
		"package claimed twice": {
			shards: []shard{
				{Name: "a", Packages: []string{"example.com/m/a"}},
				{Name: "b", Packages: []string{"example.com/m/a"}},
				{Name: "rest"},
			},
			all:     []string{"example.com/m/a"},
			wantErr: "claimed by both",
		},
		"duplicate shard name": {
			shards:  []shard{{Name: "a", Packages: []string{"example.com/m/a"}}, {Name: "a"}},
			all:     []string{"example.com/m/a"},
			wantErr: "duplicate shard name",
		},
	}

	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := partition(testCase.shards, testCase.all)
			if err == nil {
				t.Fatalf("partition succeeded, want error containing %q", testCase.wantErr)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("partition error = %q, want it to contain %q", err, testCase.wantErr)
			}
		})
	}
}

// The workflow matrix names every shard by hand, so the declared set must match
// what it was written against or a lane would stop running unnoticed.
func TestRaceShardsPartitionTheRepository(t *testing.T) {
	all, err := listPackages(moduleRoot(t))
	if err != nil {
		t.Fatalf("list packages: %v", err)
	}

	assigned, err := partition(raceShards, all)
	if err != nil {
		t.Fatalf("partition: %v", err)
	}

	total := 0
	for _, packages := range assigned {
		total += len(packages)
	}
	if total != len(all) {
		t.Fatalf("shards cover %d packages, want %d", total, len(all))
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()

	output, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("locate go.mod: %v", err)
	}
	path := strings.TrimSpace(string(output))
	if path == "" || path == os.DevNull {
		t.Fatal("no module in scope")
	}
	return filepath.Dir(path)
}
