package speech

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Model describes a speech model archive: where it comes from, its checksum and the files the
// helper needs from it.
type Model struct {
	Name        string   // folder name under <state>/speech, and the name speech.prepare reports
	URL         string   // the archive: tar, compressed with bzip2 or gzip
	SHA256      string   // hex SHA-256 of the archive
	Size        int64    // bytes of the archive, for progress
	Files       []string // base names to keep; anything else in the archive is skipped
	Attribution string   // the licence line, logged with every download and saved beside the files
}

// Parakeet is the default model: NVIDIA Parakeet TDT 0.6B v2, English, int8, as converted to
// ONNX by the sherpa-onnx project (voice.md).
var Parakeet = Model{
	Name:   "parakeet-tdt-0.6b-v2-int8",
	URL:    "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/sherpa-onnx-nemo-parakeet-tdt-0.6b-v2-int8.tar.bz2",
	SHA256: "157c157bc51155e03e37d2466522a3a737dd9c72bb25f36eb18912964161e1ad",
	Size:   482468385,
	Files:  []string{"encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx", "tokens.txt"},
	Attribution: "Parakeet TDT 0.6B v2 by NVIDIA, licensed under CC-BY-4.0 " +
		"(https://huggingface.co/nvidia/parakeet-tdt-0.6b-v2); ONNX conversion by the sherpa-onnx project (Apache-2.0).",
}

// stampFile marks a complete model folder: it holds the archive's checksum.
const stampFile = ".complete"

// attributionFile holds the model's licence line beside its files.
const attributionFile = "ATTRIBUTION.txt"

// present reports whether dir holds a complete copy of m.
func present(dir string, m Model) bool {
	b, err := os.ReadFile(filepath.Join(dir, stampFile))
	if err != nil || strings.TrimSpace(string(b)) != m.SHA256 {
		return false
	}
	for _, f := range m.Files {
		if st, err := os.Stat(filepath.Join(dir, f)); err != nil || !st.Mode().IsRegular() {
			return false
		}
	}
	return true
}

// progressFunc gets the bytes of the archive read so far.
type progressFunc func(read int64)

// fetch downloads m into dir. It streams the archive once: the bytes go through the checksum
// and the extractor together, into a temporary folder beside dir that is renamed into place
// only when the checksum matches. A failed fetch leaves no partial model.
func fetch(ctx context.Context, client *http.Client, m Model, dir string, progress progressFunc) (err error) {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+".download-")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	h := sha256.New()
	cr := &countReader{r: resp.Body, progress: progress}
	src := io.TeeReader(cr, h)
	if err := extract(src, tmp, m.Files); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, src); err != nil { // the rest of the archive, for the checksum
		return err
	}
	if m.Size > 0 && cr.n != m.Size {
		return fmt.Errorf("download: %d bytes, want %d", cr.n, m.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
		return fmt.Errorf("download: checksum %s, want %s", got, m.SHA256)
	}
	for _, f := range m.Files {
		if _, err := os.Stat(filepath.Join(tmp, f)); err != nil {
			return fmt.Errorf("download: the archive has no %s", f)
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, attributionFile), []byte(m.Attribution+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, stampFile), []byte(m.SHA256+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return os.Rename(tmp, dir)
}

type countReader struct {
	r        io.Reader
	n        int64
	progress progressFunc
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if n > 0 && c.progress != nil {
		c.progress(c.n)
	}
	return n, err
}

// extract reads a tar archive, plain or compressed with bzip2 or gzip (told apart by their
// magic bytes), and writes the regular files whose base names are in keep into dir. Paths in
// the archive never choose where a file lands: only the base name counts, so an entry such as
// ../x cannot leave dir.
func extract(r io.Reader, dir string, keep []string) error {
	br := bufio.NewReaderSize(r, 64<<10)
	magic, _ := br.Peek(3)
	var tr *tar.Reader
	switch {
	case bytes.HasPrefix(magic, []byte("BZh")):
		tr = tar.NewReader(bzip2.NewReader(br))
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		zr, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("archive: %w", err)
		}
		defer zr.Close()
		tr = tar.NewReader(zr)
	default:
		tr = tar.NewReader(br)
	}
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("archive: %w", err)
		}
		name := path.Base(hdr.Name)
		if hdr.Typeflag != tar.TypeReg || !slices.Contains(keep, name) || seen[name] {
			continue
		}
		seen[name] = true
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("archive: %s: %w", name, err)
		}
	}
}
