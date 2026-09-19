package embed

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
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
