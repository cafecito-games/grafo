package updatecheck

import "testing"

func TestComparableNormalizesReleaseSpellings(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		version string
		want    string
	}{
		{name: "bare release as stamped by goreleaser", version: "0.4.2", want: "v0.4.2"},
		{name: "tag spelling as published on github", version: "v0.4.2", want: "v0.4.2"},
		{name: "surrounding whitespace", version: "  v0.4.2  ", want: "v0.4.2"},
		{name: "prerelease", version: "0.1.0-dev", want: "v0.1.0-dev"},
		{name: "build metadata", version: "0.4.2+darwin", want: "v0.4.2+darwin"},
		{name: "empty", version: "", want: ""},
		{name: "whitespace only", version: "   ", want: ""},
		{name: "not a version", version: "latest", want: ""},
		{name: "html error page", version: "<!DOCTYPE html>", want: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Comparable(testCase.version); got != testCase.want {
				t.Fatalf("Comparable(%q) = %q, want %q", testCase.version, got, testCase.want)
			}
		})
	}
}

func TestIsReleaseExcludesDevelopmentBuilds(t *testing.T) {
	for _, testCase := range []struct {
		version string
		want    bool
	}{
		{version: "0.4.2", want: true},
		{version: "v0.4.2", want: true},
		{version: "1.0.0", want: true},
		{version: "0.1.0-dev", want: false},
		{version: "0.4.2-next.1", want: false},
		{version: "devel", want: false},
		{version: "", want: false},
	} {
		if got := IsRelease(testCase.version); got != testCase.want {
			t.Errorf("IsRelease(%q) = %t, want %t", testCase.version, got, testCase.want)
		}
	}
}

func TestNewerIgnoresUnusableVersions(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		current   string
		candidate string
		want      bool
	}{
		{name: "patch ahead", current: "0.4.1", candidate: "0.4.2", want: true},
		{name: "minor ahead across spellings", current: "0.4.2", candidate: "v0.5.0", want: true},
		{name: "equal", current: "0.4.2", candidate: "v0.4.2", want: false},
		{name: "behind", current: "0.5.0", candidate: "0.4.2", want: false},
		{name: "prerelease of the same version does not supersede", current: "0.4.2", candidate: "0.4.3-rc.1", want: true},
		{name: "release supersedes prerelease", current: "0.4.2-dev", candidate: "0.4.2", want: true},
		{name: "unusable candidate", current: "0.4.2", candidate: "not-a-version", want: false},
		{name: "unusable current", current: "", candidate: "0.4.2", want: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := Newer(testCase.current, testCase.candidate); got != testCase.want {
				t.Fatalf("Newer(%q, %q) = %t, want %t",
					testCase.current, testCase.candidate, got, testCase.want)
			}
		})
	}
}
