// Package extract pulls readable text out of common file formats so
// that search (Phase 2 keywords today, Phase 4 embeddings later) can
// see *inside* documents, not just their filenames.
//
// Phase 3 scope: .txt, .md, .pdf, .docx only. No OCR (scanned/image
// PDFs yield no text — a documented limitation, not a silent failure),
// no embeddings, no other formats.
package extract

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ledongthuc/pdf"
)

// ErrUnsupported is returned when extraction is asked of a file type
// Delve doesn't handle. An explicit error beats failing silently: the
// scanner logs it and the database records the file as not-applicable,
// so unsupported files are visible in behavior, never mysteries.
var ErrUnsupported = errors.New("content extraction not supported for this file type")

// MaxContentChars caps stored text per file. A pathological 500MB log
// must not become a 500MB database row. Truncation is rune-safe (see
// truncate) and Phase 4's embedding chunker will inherit this bound.
const MaxContentChars = 50000

// maxReadBytes bounds how much we ever pull off disk: 4 bytes per rune
// is the UTF-8 worst case, plus headroom. Reading happens through
// io.LimitReader so even this cap never fully materializes in memory
// for formats that stream (txt/md).
const maxReadBytes = int64(MaxContentChars*4 + 4096)

// supported maps lowercase-dotted extensions to "extractable". Small,
// explicit, and the single place Phase 4+ edits when formats are added.
var supported = map[string]bool{
	".txt":  true,
	".md":   true,
	".pdf":  true,
	".docx": true,
}

// IsSupported reports whether ExtractText handles this extension.
// The scanner uses it to skip unsupported files without an error.
func IsSupported(extension string) bool {
	return supported[strings.ToLower(extension)]
}

// ExtractText dispatches on extension and returns the document's
// readable text, truncated to MaxContentChars. Unsupported extensions
// return ErrUnsupported (wrapped with the offending extension).
func ExtractText(filePath string, extension string) (string, error) {
	ext := strings.ToLower(extension)
	if !IsSupported(ext) {
		return "", fmt.Errorf("%w: %s", ErrUnsupported, ext)
	}

	var text string
	var err error
	switch ext {
	case ".txt", ".md":
		text, err = extractPlainText(filePath)
	case ".pdf":
		text, err = extractPDFText(filePath)
	case ".docx":
		text, err = extractDocxText(filePath)
	}
	if err != nil {
		return "", err
	}
	return truncate(text), nil
}

// extractPlainText reads txt/md files directly — no parsing needed.
// LimitReader caps the read and ToValidUTF8 replaces any invalid bytes
// (mixed-encoding files, stray binary) with U+FFFD instead of letting
// bad UTF-8 reach SQLite or, later, the embedding model.
func extractPlainText(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	raw, err := io.ReadAll(io.LimitReader(f, maxReadBytes))
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(raw), "\uFFFD"), nil
}

// extractPDFText pulls text from a PDF using ledongthuc/pdf.
//
// LIBRARY CHOICE, with tradeoffs stated plainly: this is a pure-Go
// port in the rsc/pdf lineage — no CGO, no poppler, no system
// libraries, so the single-binary goal survives. That purity costs
// accuracy on hard PDFs: complex layouts lose reading order, Form
// XObjects and some CJK fonts decode poorly, and maintenance upstream
// is quiet (~2-3 merges/year, untagged releases). The maintained fork
// github.com/Detective-XH/gopdf is API-compatible and is the documented
// swap-in if upstream ever blocks us — only this file would change.
//
// OUT OF SCOPE, deliberately: scanned/image-only PDFs contain no text
// layer, so extraction returns "" without error. That is not a bug to
// fix here — it is the OCR gap, scheduled after embeddings, and the
// database records the attempt (empty content + timestamp) rather than
// retrying a hopeless file on every scan. Encrypted PDFs return an
// error and flow into the scanner's warn-and-continue path.
func extractPDFText(filePath string) (string, error) {
	f, reader, err := pdf.Open(filePath)
	if err != nil {
		return "", err
	}
	// pdf.Open hands us an *os.File: we own it, we close it. The
	// text itself comes from reader, a separate in-memory structure.
	defer f.Close()

	// GetPlainText streams decoded page text as an io.Reader. (This
	// version of the API takes no font-map argument — it always uses
	// the fonts embedded in the document.)
	stream, err := reader.GetPlainText()
	if err != nil {
		return "", err
	}
	raw, err := io.ReadAll(io.LimitReader(stream, maxReadBytes))
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(raw), "\uFFFD"), nil
}

// extractDocxText reads a .docx with the standard library only — no
// third-party dependency at all.
//
// DOCX STRUCTURE, explained: a .docx is not a document, it is a ZIP
// archive (Office Open XML). Inside, word/document.xml holds the main
// body as WordprocessingML: <w:p> paragraphs contain <w:r> runs which
// contain <w:t> text nodes. So "extraction" is: unzip one entry, stream
// its XML, concatenate every <w:t> character run, separating paragraphs
// with newlines. Headers, footers, footnotes, and comments live in
// sibling parts (header*.xml, footnotes.xml, ...) — deliberately
// skipped in Phase 3; document.xml covers the body users search for.
// A full OOXML library would buy tables/styles fidelity we don't need
// for keyword search and embeddings.
func extractDocxText(filePath string) (string, error) {
	zr, err := zip.OpenReader(filePath)
	if err != nil {
		// A corrupt .docx dies here (not a zip) — surfaces as a
		// scanner warning, never a crashed scan.
		return "", err
	}
	defer zr.Close()

	var docXML io.ReadCloser
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docXML, err = f.Open()
			if err != nil {
				return "", err
			}
			break
		}
	}
	if docXML == nil {
		return "", fmt.Errorf("docx missing word/document.xml: %s", filePath)
	}
	defer docXML.Close()

	// Streaming decode (not Unmarshal into a struct): document.xml can
	// be megabytes, and we only want one element type. We track <w:p>
	// boundaries so paragraphs don't glue into one long word.
	decoder := xml.NewDecoder(io.LimitReader(docXML, maxReadBytes))
	var out strings.Builder
	inText := false
	for {
		tok, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			// Namespaces vary (w:, w14:, bare) across producers, so
			// match the local name only: "t" = text, "p" = paragraph,
			// "tab"/"br" preserve minimal whitespace structure.
			switch tok.Name.Local {
			case "t":
				inText = true
			case "p":
				if out.Len() > 0 {
					out.WriteByte('\n')
				}
			case "tab":
				out.WriteByte('\t')
			case "br":
				out.WriteByte('\n')
			}
		case xml.EndElement:
			if tok.Name.Local == "t" {
				inText = false
			}
		case xml.CharData:
			if inText {
				out.Write([]byte(tok))
			}
		}
		// Safety valve: stop accumulating past the byte cap even if
		// the XML runs on (truncate() enforces the rune cap after).
		if int64(out.Len()) > maxReadBytes {
			break
		}
	}
	return strings.ToValidUTF8(out.String(), "\uFFFD"), nil
}

// truncate caps text at MaxContentChars on a rune boundary. Naive
// slicing text[:n] can split a multi-byte UTF-8 sequence and produce
// invalid strings — converting to []rune first slices by character,
// never by byte.
func truncate(text string) string {
	runes := []rune(text)
	if len(runes) <= MaxContentChars {
		return text
	}
	return string(runes[:MaxContentChars])
}
