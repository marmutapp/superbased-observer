// Package analyze computes architecture overviews, Louvain community
// detection, dead-code detection, and git-diff impact over a node/edge
// graph passed in as plain data. It is PURE — no database/sql,
// net/http, or fsnotify; the store seam loads the graph and the
// surfaces present the results. See docs/codeintel/analysis.md.
package analyze
