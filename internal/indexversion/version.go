// Package indexversion owns the compatibility version of Grafo's persisted
// semantic graph. Storage readers and the indexer share this authority so a
// query-only open cannot accept a graph the writer would rebuild.
package indexversion

const Semantic = "37"
