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
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/model/wordpiece"
	"github.com/sugarme/tokenizer/normalizer"
	"github.com/sugarme/tokenizer/pretokenizer"
	"github.com/sugarme/tokenizer/processor"
	ort "github.com/yalue/onnxruntime_go"
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

	// ortVersion pins the ONNX Runtime release whose C ABI matches the
	// headers vendored by onnxruntime_go v1.36 (ORT_API_VERSION 29 =
	// second version component). Wrapper and library MUST agree —
	// mismatches fail at InitializeEnvironment with an obscure error,
	// so this constant is the single place both sides meet.
	ortVersion = "1.29.0"

	modelRepo = "Xenova/all-MiniLM-L6-v2"
	// Quantized int8 export: same retrieval quality as fp32 for search
	// purposes at 1/4 the download (23MB vs 90MB).
	modelAsset = "onnx/model_quantized.onnx"
	vocabAsset = "vocab.txt"
)

// dirs returns (modelsDir, libDir): ~/.delve/models/minilm and
// ~/.delve/lib. Split because they cache different things with
// different versioning (model repo vs ORT release).
func dirs() (modelsDir, libDir string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	base := filepath.Join(home, ".delve")
	return filepath.Join(base, "models", "minilm"), filepath.Join(base, "lib"), nil
}

// ortAsset maps the current OS/arch to its upstream ONNX Runtime
// archive and the shared-library file inside it. V1 covers the common
// dev machines; anything else fails with a clear error instead of a
// broken download URL.
func ortAsset() (archiveURL, libFile, localName string, err error) {
	base := fmt.Sprintf("https://github.com/microsoft/onnxruntime/releases/download/v%s", ortVersion)
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return base + "/onnxruntime-linux-x64-" + ortVersion + ".tgz",
			"lib/libonnxruntime.so." + ortVersion, "libonnxruntime.so", nil
	case "linux/arm64":
		return base + "/onnxruntime-linux-aarch64-" + ortVersion + ".tgz",
			"lib/libonnxruntime.so." + ortVersion, "libonnxruntime.so", nil
	case "darwin/arm64":
		return base + "/onnxruntime-osx-arm64-" + ortVersion + ".tgz",
			"lib/libonnxruntime." + ortVersion + ".dylib", "libonnxruntime.dylib", nil
	case "darwin/amd64":
		return base + "/onnxruntime-osx-x86_64-" + ortVersion + ".tgz",
			"lib/libonnxruntime." + ortVersion + ".dylib", "libonnxruntime.dylib", nil
	case "windows/amd64":
		return base + "/onnxruntime-win-x64-" + ortVersion + ".zip",
			"lib/onnxruntime.dll", "onnxruntime.dll", nil
	default:
		return "", "", "", fmt.Errorf("no prebuilt ONNX Runtime %s for %s/%s — semantic search needs a supported platform",
			ortVersion, runtime.GOOS, runtime.GOARCH)
	}
}

// EnsureModel guarantees the model, vocab, and runtime library exist
// locally, downloading each exactly once with visible progress. After
// this returns, everything runs offline. Safe to call repeatedly —
// present files are never re-downloaded.
func EnsureModel() (modelPath, vocabPath, libPath string, err error) {
	modelsDir, libDir, err := dirs()
	if err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return "", "", "", err
	}

	hfBase := "https://huggingface.co/" + modelRepo + "/resolve/main/"
	modelPath = filepath.Join(modelsDir, "model_quantized.onnx")
	vocabPath = filepath.Join(modelsDir, "vocab.txt")
	if err := downloadOnce(hfBase+modelAsset, modelPath, "embedding model"); err != nil {
		return "", "", "", err
	}
	if err := downloadOnce(hfBase+vocabAsset, vocabPath, "model vocabulary"); err != nil {
		return "", "", "", err
	}

	archiveURL, libFile, localName, err := ortAsset()
	if err != nil {
		return "", "", "", err
	}
	libPath = filepath.Join(libDir, localName)
	if _, err := os.Stat(libPath); err == nil {
		fmt.Fprintf(os.Stderr, "embedding runtime ready (cached)\n")
	} else {
		if err := downloadLibOnce(archiveURL, libFile, libPath); err != nil {
			return "", "", "", err
		}
	}
	return modelPath, vocabPath, libPath, nil
}

// downloadOnce fetches url to dest unless dest already exists.
// Atomic write (temp file + rename) so an interrupted download never
// leaves a half-file that looks complete on the next run.
func downloadOnce(url, dest, label string) error {
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		fmt.Fprintf(os.Stderr, "%s ready (cached)\n", label)
		return nil
	}
	fmt.Fprintf(os.Stderr, "downloading %s (one-time setup)...\n", label)
	tmp := dest + ".part"
	if err := downloadWithProgress(url, tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("downloading %s: %w", label, err)
	}
	fmt.Fprintf(os.Stderr, "\n")
	return os.Rename(tmp, dest)
}

// downloadLibOnce fetches the ORT archive and extracts just the shared
// library. Only one file out of the archive is needed, so we stream
// entries and copy the match — never unpacking the whole tree.
func downloadLibOnce(archiveURL, libFile, dest string) error {
	fmt.Fprintf(os.Stderr, "downloading embedding runtime (one-time setup)...\n")
	tmp, err := os.CreateTemp("", "ort-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := downloadWithProgress(archiveURL, tmpName); err != nil {
		tmp.Close()
		return fmt.Errorf("downloading embedding runtime: %w", err)
	}
	tmp.Close()
	fmt.Fprintf(os.Stderr, "\n")

	var extractErr error
	if strings.HasSuffix(archiveURL, ".zip") {
		extractErr = extractFromZip(tmpName, libFile, dest)
	} else {
		extractErr = extractFromTgz(tmpName, libFile, dest)
	}
	if extractErr != nil {
		return fmt.Errorf("extracting embedding runtime: %w", extractErr)
	}
	return nil
}

// progressWriter prints "label-agnostic" byte progress to stderr:
// "  12.4 / 23.0 MB (54%)" rewritten in place via \r.
type progressWriter struct {
	total   int64
	written int64
	last    time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.written += int64(len(b))
	if time.Since(p.last) > 200*time.Millisecond || p.written == p.total {
		p.last = time.Now()
		const mb = 1024 * 1024
		if p.total > 0 {
			fmt.Fprintf(os.Stderr, "\r  %.1f / %.1f MB (%d%%)",
				float64(p.written)/mb, float64(p.total)/mb, p.written*100/p.total)
		} else {
			fmt.Fprintf(os.Stderr, "\r  %.1f MB downloaded", float64(p.written)/mb)
		}
	}
	return len(b), nil
}

// downloadWithProgress streams url to dest with a live progress line.
// A modest timeout keeps a stalled first-run from hanging forever.
func downloadWithProgress(url, dest string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected HTTP status: %s", resp.Status)
	}
	_, err = io.Copy(out, io.TeeReader(resp.Body, &progressWriter{total: resp.ContentLength}))
	return err
}

// extractFromTgz copies a single entry out of a .tgz archive.
func extractFromTgz(archive, entry, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("library file %s not found in archive", entry)
		}
		if err != nil {
			return err
		}
		// Archives may prefix paths (e.g. "./lib/..."); match the tail.
		if hdr.Name == entry || strings.HasSuffix(hdr.Name, "/"+filepath.Base(entry)) && strings.Contains(hdr.Name, "lib/") {
			return writeFile(dest, tr, 0o755)
		}
	}
}

// extractFromZip copies a single entry out of a .zip archive.
func extractFromZip(archive, entry, dest string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == entry {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeFile(dest, rc, 0o755)
		}
	}
	return fmt.Errorf("library file %s not found in archive", entry)
}

// writeFile drains src into dest atomically (temp + rename), like the
// model downloads above.
func writeFile(dest string, src io.Reader, perm os.FileMode) error {
	tmp := dest + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, src)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, dest)
}

// ---------------------------------------------------------------------------
// Session + inference
// ---------------------------------------------------------------------------

var (
	sessionOnce sync.Once
	sessionErr  error
	embedder    *session
)

type session struct {
	ortSession *ort.AdvancedSession
	tokenizer  *tokenizer.Tokenizer
	inputNames []string
	hasTypeIDs bool
	inputIDs   *ort.Tensor[int64]
	attnMask   *ort.Tensor[int64]
	typeIDs    *ort.Tensor[int64]
	output     *ort.Tensor[float32]
}

// ensureSession downloads (once), loads the runtime, builds the BERT
// tokenizer pipeline, and creates a single session with FIXED input
// shape [1, MaxTokens]. Fixed shape matters: onnxruntime_go sessions
// own caller-provided tensors, so variable-length inputs would force
// session rebuilds per text. Instead every input is padded/truncated
// to MaxTokens once, and the same tensors are refilled per call
// (guarded by mutex — the scanner is single-threaded, but cheap
// insurance for future parallel phases).
var sessionMu sync.Mutex

func ensureSession() (*session, error) {
	sessionOnce.Do(func() {
		sessionErr = initSession()
	})
	if sessionErr != nil {
		return nil, sessionErr
	}
	return embedder, nil
}

func initSession() error {
	modelPath, vocabPath, libPath, err := EnsureModel()
	if err != nil {
		return err
	}

	ort.SetSharedLibraryPath(libPath)
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("starting embedding runtime (library/runtime mismatch? expected ORT %s): %w", ortVersion, err)
	}
	// Short-lived CLI: the OS reclaims the ORT environment at exit.
	// A long-lived daemon phase would call ort.DestroyEnvironment().

	tk, err := buildTokenizer(vocabPath)
	if err != nil {
		return err
	}

	// Probe the model's declared inputs instead of assuming them:
	// some MiniLM exports include token_type_ids, some don't.
	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return fmt.Errorf("inspecting embedding model: %w", err)
	}
	names := map[string]bool{}
	for _, in := range inputs {
		names[in.Name] = true
	}
	var inputNames []string
	for _, want := range []string{"input_ids", "attention_mask", "token_type_ids"} {
		if names[want] {
			inputNames = append(inputNames, want)
		}
	}
	if !names["input_ids"] || !names["attention_mask"] {
		return fmt.Errorf("embedding model missing required inputs (has %v)", inputNames)
	}
	outName := "last_hidden_state"
	found := false
	for _, out := range outputs {
		if out.Name == outName {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("embedding model missing %q output", outName)
	}

	shape := ort.NewShape(1, MaxTokens)
	inputIDs, err := ort.NewEmptyTensor[int64](shape)
	if err != nil {
		return err
	}
	attnMask, err := ort.NewEmptyTensor[int64](shape)
	if err != nil {
		return err
	}
	var typeIDs *ort.Tensor[int64]
	tensors := []*ort.Tensor[int64]{inputIDs, attnMask}
	hasTypeIDs := false
	for _, n := range inputNames {
		if n == "token_type_ids" {
			typeIDs, err = ort.NewEmptyTensor[int64](shape)
			if err != nil {
				return err
			}
			tensors = append(tensors, typeIDs)
			hasTypeIDs = true
		}
	}
	// MiniLM has no pooler: output is per-token hidden states
	// [1, MaxTokens, Dim], pooled + normalized in Go below.
	output, err := ort.NewEmptyTensor[float32](ort.NewShape(1, MaxTokens, Dim))
	if err != nil {
		return err
	}
	// []ort.Value conversion: Tensor implements the Value interface;
	// (nil options would also work, but explicit defaults document that
	// we run plain CPU with no execution-provider tweaks.)
	var inVals []ort.Value
	for _, t := range tensors {
		inVals = append(inVals, t)
	}
	options, err := ort.NewSessionOptions()
	if err != nil {
		return fmt.Errorf("creating session options: %w", err)
	}
	sess, err := ort.NewAdvancedSession(modelPath, inputNames, []string{outName}, inVals, []ort.Value{output}, options)
	if err != nil {
		return fmt.Errorf("loading embedding model: %w", err)
	}
	embedder = &session{
		ortSession: sess,
		tokenizer:  tk,
		inputNames: inputNames,
		hasTypeIDs: hasTypeIDs,
		inputIDs:   inputIDs,
		attnMask:   attnMask,
		typeIDs:    typeIDs,
		output:     output,
	}
	return nil
}

// buildTokenizer assembles the BERT WordPiece pipeline MiniLM was
// trained with: lowercase + accent-strip + CJK-aware normalization,
// BERT pre-tokenization (whitespace/punctuation split), WordPiece
// subwords from the model's own vocab.txt, and [CLS]/[SEP] framing.
// Tokenizer and model MUST agree — mismatched tokenization silently
// degrades every embedding, which is why the vocab downloads with the
// model instead of being vendored.
func buildTokenizer(vocabPath string) (*tokenizer.Tokenizer, error) {
	model, err := wordpiece.NewWordPieceFromFile(vocabPath, "[UNK]")
	if err != nil {
		return nil, fmt.Errorf("loading model vocabulary: %w", err)
	}
	tk := tokenizer.NewTokenizer(model)
	// cleanText, handleChineseChars, stripAccents, lowercase — the
	// bert-base-uncased recipe MiniLM inherits.
	tk.WithNormalizer(normalizer.NewBertNormalizer(true, true, true, true))
	tk.WithPreTokenizer(pretokenizer.NewBertPreTokenizer())

	sepID, ok := tk.TokenToId("[SEP]")
	if !ok {
		return nil, fmt.Errorf("vocabulary missing [SEP] token")
	}
	clsID, ok := tk.TokenToId("[CLS]")
	if !ok {
		return nil, fmt.Errorf("vocabulary missing [CLS] token")
	}
	tk.WithPostProcessor(processor.NewBertProcessing(
		processor.PostToken{Id: sepID, Value: "[SEP]"},
		processor.PostToken{Id: clsID, Value: "[CLS]"},
	))
	tk.WithTruncation(&tokenizer.TruncationParams{
		MaxLength: MaxTokens,
		Strategy:  tokenizer.LongestFirst,
		Stride:    0,
	})
	return tk, nil
}

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

	// Mean pool over real (masked-in) tokens, then L2-normalize.
	hidden := s.output.GetData() // [MaxTokens*Dim], row-major
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
