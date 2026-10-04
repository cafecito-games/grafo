module github.com/cafecito-games/grafo

go 1.26.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/bufbuild/protocompile v0.14.1
	github.com/cafecito-games/gdparser v0.1.6
	// A fork of alex-pinkus/tree-sitter-swift, which publishes the generated
	// parser sources upstream declines to commit. The fork renames the module
	// after itself so this requirement needs no replace directive, which would
	// make `go install github.com/cafecito-games/grafo/cmd/grafo@version` fail.
	github.com/cafecito-games/tree-sitter-swift v0.7.3-grafo.1
	github.com/cockroachdb/pebble/v2 v2.1.7
	github.com/mattn/go-isatty v0.0.24
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/pganalyze/pg_query_go/v6 v6.2.2
	github.com/pressly/goose/v3 v3.28.0
	github.com/sqlc-dev/meyer v0.1.2
	github.com/tree-sitter/go-tree-sitter v0.25.0
	github.com/tree-sitter/tree-sitter-java v0.23.5
	github.com/tree-sitter/tree-sitter-python v0.25.0
	github.com/tree-sitter/tree-sitter-typescript v0.23.2
	github.com/yuin/goldmark v1.8.6
	go.etcd.io/bbolt v1.5.0
	golang.org/x/mod v0.39.0
	golang.org/x/sys v0.47.0
	golang.org/x/tools v0.49.0
	google.golang.org/protobuf v1.36.12
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/DataDog/zstd v1.5.7 // indirect
	github.com/RaduBerinde/axisds v0.1.0 // indirect
	github.com/RaduBerinde/btreemap v0.0.0-20250419174037-3d62b7205d54 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cockroachdb/crlib v0.0.0-20241112164430-1264a2edc35b // indirect
	github.com/cockroachdb/errors v1.11.3 // indirect
	github.com/cockroachdb/logtags v0.0.0-20230118201751-21c54148d20b // indirect
	github.com/cockroachdb/redact v1.1.5 // indirect
	github.com/cockroachdb/swiss v0.0.0-20260820225851-333444432258 // indirect
	github.com/cockroachdb/tokenbucket v0.0.0-20230807174530-cc333fc44b06 // indirect
	github.com/getsentry/sentry-go v0.27.0 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/golang/snappy v0.0.5-0.20231225225746-43d5d4cd4e0e // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-pointer v0.0.1 // indirect
	github.com/matttproud/golang_protobuf_extensions v1.0.4 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/minio/minlz v1.0.1-0.20250507153514-87eb42fe8882 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/prometheus/client_golang v1.16.0 // indirect
	github.com/prometheus/client_model v0.3.0 // indirect
	github.com/prometheus/common v0.42.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	github.com/rogpeppe/go-internal v1.9.0 // indirect
	github.com/segmentio/asm v1.2.1 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/exp v0.0.0-20260824195058-e88cd73687aa // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
