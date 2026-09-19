// Topic clustering for the organize suggestion engine: greedy leader
// clustering over file embedding vectors.
//
// WHY GREEDY-LEADER, NOT K-MEANS: k-means needs k guessed up front,
// starts from random seeds (nondeterministic), and hides *why* two
// files group together. Greedy-leader needs only a similarity
// threshold, visits files in sorted-path order (fully deterministic),
// and each cluster is explainable: every member sits within
// `threshold` cosine of the running rep.
//
// THRESHOLD SEMANTICS (cosine vs the cluster rep): ~0.75 is the
// default; ~0.70 groups broadly (related topics merged); ~0.80 keeps
// near-duplicates only. Unrelated MiniLM vectors sit ~0.1,
// paraphrases ~0.9.
package organize

import (
	"fmt"
	"math"
	"sort"

	"github.com/NoahMenezes/Delve/internal/db"
	"github.com/NoahMenezes/Delve/internal/search"
)

// cluster is one topic: member files plus rep, the L2-normalized
// centroid of the member vectors — the exemplar direction newcomers
// are compared against via search.Cosine.
type cluster struct {
	members []db.StoredEmbedding
	rep     []float32
}

// clusterEmbeddings greedily groups embeddings: each file joins the
// most similar cluster whose rep scores >= threshold, else starts a
// new one. The rep is recomputed as the normalized centroid on every
// join, so later comparisons track the whole cluster, not just its
// first file. Input is sorted by path for determinism.
//
// Empty vectors carry no signal and are skipped silently; a non-empty
// vector whose dimension disagrees with the first aborts with an error
// naming the path (same loud-failure policy as SemanticSearch on model
// swaps). NOTE: the (, error) return deviates from a bare []cluster so
// incompatible vector spaces fail loudly instead of mis-clustering.
func clusterEmbeddings(embs []db.StoredEmbedding, threshold float64) ([]cluster, error) {
	sort.Slice(embs, func(i, j int) bool { return embs[i].Record.Path < embs[j].Record.Path })
	var dim int
	var seen bool
	var out []cluster
	for _, se := range embs {
		if len(se.Vector) == 0 {
			continue
		}
		if !seen {
			dim, seen = len(se.Vector), true
		} else if len(se.Vector) != dim {
			return nil, fmt.Errorf("embedding dimension mismatch for %s: got %d, want %d (re-scan to re-embed)", se.Record.Path, len(se.Vector), dim)
		}
		best, bestScore := -1, 0.0
		for i := range out {
			if s := search.Cosine(se.Vector, out[i].rep); s >= threshold && (best < 0 || s > bestScore) {
				best, bestScore = i, s
			}
		}
		if best < 0 {
			out = append(out, cluster{members: []db.StoredEmbedding{se}, rep: normalize(se.Vector, nil)})
			continue
		}
		out[best].members = append(out[best].members, se)
		out[best].rep = normalize(centroid(out[best].members), out[best].rep)
	}
	return out, nil
}

// centroid is the mean vector of the members (shared dim is enforced
// by clusterEmbeddings before any join happens).
func centroid(members []db.StoredEmbedding) []float32 {
	mean := make([]float32, len(members[0].Vector))
	for _, m := range members {
		for i, v := range m.Vector {
			mean[i] += v
		}
	}
	for i := range mean {
		mean[i] /= float32(len(members))
	}
	return mean
}

// normalize scales v to unit length for cosine-via-dot-product
// comparison. A zero vector has no direction: keep old (the pre-join
// rep) instead of dividing by zero and producing NaNs.
func normalize(v, old []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return old
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}
