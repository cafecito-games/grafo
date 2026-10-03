package version

import (
	"runtime/debug"
	"testing"
)

func TestFromBuildInfo(t *testing.T) {
	tests := []struct {
		name   string
		info   *debug.BuildInfo
		want   string
		wantOK bool
	}{
		{name: "missing info", info: nil},
		{name: "unset version", info: buildInfo("")},
		{name: "development placeholder", info: buildInfo("(devel)")},
		{name: "module version", info: buildInfo("v1.4.0"), want: "1.4.0", wantOK: true},
		{name: "pseudo version", info: buildInfo("v0.0.0-20260101120000-abcdef123456"), want: "0.0.0-20260101120000-abcdef123456", wantOK: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := fromBuildInfo(test.info)
			if ok != test.wantOK || got != test.want {
				t.Fatalf("fromBuildInfo() = %q, %v; want %q, %v", got, ok, test.want, test.wantOK)
			}
		})
	}
}

func buildInfo(moduleVersion string) *debug.BuildInfo {
	return &debug.BuildInfo{Main: debug.Module{Path: "github.com/cafecito-games/grafo", Version: moduleVersion}}
}
