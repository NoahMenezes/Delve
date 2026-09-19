// Package embed runs a small sentence-embedding model locally (CPU)
// and turns document text into meaning-vectors for semantic search.
//
// MODEL CHOICE, with tradeoffs stated plainly: all-MiniLM-L6-v2 via
// the Xenova ONNX export (quantized, 23MB, 384 dims, Apache-2.0).
// MiniLM is the standard quality/size sweet spot for on-device
// retrieval: 6 layers run in single-digit milliseconds on a CPU,
// while 384-dim vectors keep the SQLite BLOBs small (1.5KB/file).
// Bigger models (mpnet, bge-base) score slightly higher but cost
// 4-10x the RAM and download for marginal gain on filenames+docs.
//
// RUNTIME CHOICE: github.com/yalue/onnxruntime_go (MIT) driving
// Microsoft's ONNX Runtime. Deliberately NOT pure Go: the only pure-Go
// transformer runner found is unproven (1 star) and 20-100x slower,
// which would make scan-time embedding of real corpora unusable.
// The cost is honest: onnxruntime_go needs CGO at build time (already
// true via go-sqlite3) and a version-pinned libonnxruntime shared
// library at RUNTIME. To preserve "download one file, run it", Delve
// downloads that .so once into ~/.delve/lib/ on first embed — the
// same one-time-cache pattern as the model itself. Distribution stays
// a single binary; machines without prior setup just pay one
// first-run download (model 23MB + runtime ~30MB).
package embed

import (
	"fmt"
	"math"
	"strings"

	"github.com/sugarme/tokenizer"
)

const (
	// Dim is the embedding width of all-MiniLM-L6-v2. Stored vectors
	// and query vectors must always agree on this; the DB layer
	// validates it on read.
	Dim = 384

	// MaxTokens caps the tokenized input length. MiniLM supports 512,
	// but file-search text is front-loaded (titles, first paragraphs),
	// and 256 halves the matmul cost. Longer texts truncate.
	MaxTokens = 256
)

// GenerateEmbedding turns text into a 384-dim L2-normalized meaning
// vector. Normalization happens here, at write time, so cosine
// similarity later reduces to a plain dot product.
//
// INFERENCE RECIPE, explained: BERT-family encoders output one vector
// per input token (shape [tokens, 384]), not one per text. The
// sentence-transformers standard converts this to a single vector by
// MEAN POOLING — averaging token vectors weighted by the attention
// mask (real tokens count, padding doesn't, [CLS]/[SEP] included per
// the reference recipe) — then L2-NORMALIZING (dividing by the vector
// length) so every embedding lives on the unit sphere.
func GenerateEmbedding(text string) ([]float32, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("cannot embed empty text")
	}
	s, err := ensureSession()
	if err != nil {
		return nil, err
	}

	enc, err := s.tokenizer.Encode(tokenizer.NewSingleEncodeInput(tokenizer.NewInputSequence(text)), true)
	if err != nil {
		return nil, fmt.Errorf("tokenizing text: %w", err)
	}
	n := len(enc.Ids)
	if n == 0 {
		return nil, fmt.Errorf("text produced no tokens")
	}
	if n > MaxTokens {
		n = MaxTokens // truncation params should prevent this; belt-and-suspenders
	}

	sessionMu.Lock()
	defer sessionMu.Unlock()

	ids := s.inputIDs.GetData()
	mask := s.attnMask.GetData()
	for i := 0; i < MaxTokens; i++ {
		ids[i], mask[i] = 0, 0 // [PAD]=0; clear previous call's data
	}
	for i := 0; i < n; i++ {
		ids[i] = int64(enc.Ids[i])
		mask[i] = int64(enc.AttentionMask[i])
	}
	if s.hasTypeIDs {
		zeros := s.typeIDs.GetData()
		for i := range zeros {
			zeros[i] = 0
		}
	}

	if err := s.ortSession.Run(); err != nil {
		return nil, fmt.Errorf("running embedding model: %w", err)
	}

	hidden := s.output.GetData() // [MaxTokens*Dim], row-major
	return meanPoolNormalize(hidden, mask)
}

func meanPoolNormalize(hidden []float32, mask []int64) ([]float32, error) {
	// Mean pool over real (masked-in) tokens, then L2-normalize.
	sums := make([]float64, Dim)
	var count float64
	for i := 0; i < MaxTokens; i++ {
		if mask[i] == 0 {
			continue
		}
		count++
		row := hidden[i*Dim : (i+1)*Dim]
		for d := 0; d < Dim; d++ {
			sums[d] += float64(row[d])
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("text produced no embeddable tokens")
	}
	out := make([]float32, Dim)
	var norm float64
	for d := 0; d < Dim; d++ {
		out[d] = float32(sums[d] / count)
		norm += float64(out[d]) * float64(out[d])
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return nil, fmt.Errorf("embedding collapsed to zero vector")
	}
	for d := range out {
		out[d] /= float32(norm)
	}
	return out, nil
}
