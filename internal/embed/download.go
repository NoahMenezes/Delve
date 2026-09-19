package embed

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
