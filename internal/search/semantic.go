// Package search implements meaning-based retrieval over Delve's
// local embedding index. Keyword search lives in db.SearchFiles
// (FTS5); this package is the semantic complement — same results
// shape idea, different notion of relevance.
package search

import (
	"database/sql"
	"fmt"
	"sort"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/embed"
)

// Hit is one semantic result: the file's metadata plus its cosine
// similarity to the query in [-1, 1]. Higher is more similar; in
// practice MiniLM scores land ~0.1 (unrelated) to ~0.9 (paraphrase).
// NOTE on the shape: the Phase 4 brief sketched []FileRecord, but a
// score column is what makes semantic output interpretable (and lets
// callers threshold later), so the score rides along explicitly.
type Hit struct {
	Record db.FileRecord
	Score  float64
}

// SemanticSearch embeds the query and ranks every stored file
// embedding by cosine similarity, returning the top `limit` hits.
//
// BRUTE FORCE, deliberately: each query scans all N vectors at
// O(N*dim) with a dot product — microseconds per file, milliseconds
// for thousands. Correct, dependency-free, and exactly right for a
// personal index. It will NOT scale to millions of vectors; the
// honest upgrade path is an ANN index (HNSW) or SQLite-vec, but that
// trades simplicity for a problem Delve doesn't have yet.
func SemanticSearch(database *sql.DB, query string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 20 // same sane default as keyword search
	}
	queryVec, err := embed.GenerateEmbedding(query)
	if err != nil {
		return nil, fmt.Errorf("embedding query: %w", err)
	}

	stored, err := db.GetAllEmbeddings(database)
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, nil // nothing embedded yet — caller prints the friendly empty message
	}

	hits := make([]Hit, 0, len(stored))
	for _, se := range stored {
		// Dimension mismatch means the model changed since these
		// vectors were stored: skip loudly-visible (score math would
		// otherwise silently compare incompatible spaces).
		if len(se.Vector) != len(queryVec) {
			return nil, fmt.Errorf("embedding dimension mismatch for %s: stored %d, query %d (re-scan to re-embed)",
				se.Record.Path, len(se.Vector), len(queryVec))
		}
		hits = append(hits, Hit{Record: se.Record, Score: Cosine(queryVec, se.Vector)})
	}

	// Descending score — most similar first (opposite of FTS's
	// ascending bm25; both mean "best first", different scales).
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// Cosine returns the cosine similarity of two vectors: the cosine of
// the angle between them, 1 = identical direction, 0 = orthogonal
// (unrelated), -1 = opposite. Formula: dot(a,b) / (|a|*|b|).
//
// Exported (not just used by SemanticSearch) so organize clustering
// shares the single definition of "similar" — one source of truth.
//
// The shortcut that makes this cheap: embed.GenerateEmbedding L2-
// normalizes every stored vector at write time (|v| = 1), so the
// denominator vanishes and cosine reduces to a plain dot product.
// One loop, one multiply-add per dimension, no sqrt per comparison.
// Callers must pass equal-length L2-normalized vectors.
func Cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}
