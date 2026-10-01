package version

// Value is the version Grafo reports to users and stamps into index metadata.
// Release builds replace it through -ldflags; the default covers local builds
// and `go install`.
var Value = "0.1.0-dev"
