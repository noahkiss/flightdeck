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
	"time"
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

// errStalled ends a download that received no bytes for the stall time.
var errStalled = errors.New("stalled")

// maxModelFile caps one extracted file when the model gives no archive size.
const maxModelFile = 4 << 30

// fetch downloads m into dir. It streams the archive once: the bytes go through the checksum
// and the extractor together, into a temporary folder beside dir that is renamed into place
// only when the checksum matches. A failed fetch leaves no partial model, and a fetch first
// removes what an earlier one cut off by a restart left behind. The download fails when no
// bytes arrive for stall, when the archive runs past m.Size, or when one file does.
func fetch(ctx context.Context, client *http.Client, m Model, dir string, stall time.Duration, progress progressFunc) (err error) {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	prefix := "." + filepath.Base(dir) + ".download-"
	if old, _ := filepath.Glob(filepath.Join(parent, prefix+"*")); len(old) > 0 {
		for _, o := range old { // one gff-hab owns a state folder: these are no one's
			os.RemoveAll(o)
		}
	}
	tmp, err := os.MkdirTemp(parent, prefix)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
		}
	}()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(stall, func() { cancel(errStalled) })
	defer watchdog.Stop()
	defer func() {
		if err != nil && errors.Is(context.Cause(ctx), errStalled) {
			err = fmt.Errorf("download stalled: no bytes for %v", stall)
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
	cr := &countReader{r: resp.Body, limit: m.Size, progress: func(n int64) {
		watchdog.Reset(stall)
		if progress != nil {
			progress(n)
		}
	}}
	src := io.TeeReader(cr, h)
	maxFile := int64(maxModelFile)
	if m.Size > 0 {
		maxFile = m.Size
	}
	if err := extract(src, tmp, m.Files, maxFile); err != nil {
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

// countReader counts the archive's bytes, and fails once they pass limit (when limit > 0).
type countReader struct {
	r        io.Reader
	n        int64
	limit    int64
	progress progressFunc
}

func (c *countReader) Read(p []byte) (int, error) {
	if c.limit > 0 && int64(len(p)) > c.limit+1-c.n {
		p = p[:c.limit+1-c.n] // read at most one byte past the limit
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.limit > 0 && c.n > c.limit {
		return n, fmt.Errorf("download: more than %d bytes", c.limit)
	}
	if n > 0 && c.progress != nil {
		c.progress(c.n)
	}
	return n, err
}

// extract reads a tar archive, plain or compressed with bzip2 or gzip (told apart by their
// magic bytes), and writes the regular files whose base names are in keep into dir. Paths in
// the archive never choose where a file lands: only the base name counts, so an entry such as
// ../x cannot leave dir.
func extract(r io.Reader, dir string, keep []string, maxFile int64) error {
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
		var n int64
		n, err = io.Copy(f, io.LimitReader(tr, maxFile+1)) // compressed bytes can expand without bound
		if err == nil && n > maxFile {
			err = fmt.Errorf("larger than %d bytes", maxFile)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("archive: %s: %w", name, err)
		}
	}
}
