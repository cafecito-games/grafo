package version

import (
	"runtime/debug"
	"strings"
)

// fallback is reported when neither -ldflags nor the embedded build metadata
// names a version.
const fallback = "0.1.0-dev"

// Value is the version Grafo reports to users and stamps into index metadata.
// Release builds replace it through -ldflags; otherwise it comes from the
// build metadata the Go toolchain embeds, and only then falls back.
var Value = fallback

func init() {
	if Value != fallback {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if embedded, ok := fromBuildInfo(info); ok {
			Value = embedded
		}
	}
}

// fromBuildInfo reports the main module's version as recorded by the toolchain.
// `go install module@version` stamps the resolved module version there, which
// is the only version information such a build carries, and builds inside a
// checkout get a version derived from VCS metadata. Builds with neither leave a
// placeholder, which this rejects so the caller keeps the fallback.
func fromBuildInfo(info *debug.BuildInfo) (string, bool) {
	if info == nil {
		return "", false
	}
	embedded := strings.TrimPrefix(info.Main.Version, "v")
	if embedded == "" || embedded == "(devel)" || embedded == "devel" {
		return "", false
	}
	return embedded, true
}
