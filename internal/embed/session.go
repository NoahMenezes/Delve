package embed

import (
	"fmt"
	"sync"

	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/model/wordpiece"
	"github.com/sugarme/tokenizer/normalizer"
	"github.com/sugarme/tokenizer/pretokenizer"
	"github.com/sugarme/tokenizer/processor"
	ort "github.com/yalue/onnxruntime_go"
)

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
	// NOTE: no inputNames field — the model input list is only needed
	// transiently in initSession to build the session; per-call
	// inference branches on hasTypeIDs instead.
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
