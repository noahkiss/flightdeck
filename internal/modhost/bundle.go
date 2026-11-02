// Package modhost runs modules: it loads a module bundle (manifest.toml and main.js), runs it in
// its own goja runtime with the bridge of docs/specs/modules.md 7, under the limits of 8, and
// returns block trees checked by modules.ValidateTree. The module host process (modules.md 4,
// stage 5) holds one Module per enabled module; the backends the bridge reaches (the hab API,
// programs, HTTP, state storage) are injected. Pure Go. Spec: docs/specs/modules.md 5.1, 7.3, 8.4.
package modhost

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/noahkiss/flightdeck/api/modules"
)

// MainFile is the module's entry file (modules.md 5).
const MainFile = "main.js"

// Bundle limits (modules.md 5.1).
const (
	MaxSourceFiles = 64
	MaxSourceBytes = 1 << 20
)

// Bundle is a module folder read into memory: its manifest and every .js file in it. Nothing is
// read from disk after Load; require resolves against Sources only.
type Bundle struct {
	Dir      string
	Manifest *modules.Manifest
	// Sources maps a slash path inside the folder ("main.js", "lib/util.js") to its text.
	Sources map[string]string
}

// NewerBlocks is the error Load returns for a module whose blocks version is above the
// vocabulary this hab knows: the hab cannot check its trees.
type NewerBlocks struct {
	Blocks int64 // the version the manifest names
}

func (e *NewerBlocks) Error() string {
	return fmt.Sprintf("the module needs block vocabulary %d; this hab knows %d: it needs a newer hab", e.Blocks, modules.Version)
}

// BundleError is a module folder Load cannot use: a missing or bad file, or a limit passed.
type BundleError struct {
	Path    string // inside the folder; empty for the folder itself
	Message string
}

func (e *BundleError) Error() string {
	if e.Path == "" {
		return "module bundle: " + e.Message
	}
	return "module bundle: " + e.Path + ": " + e.Message
}

// Load reads a module folder. It parses the manifest first, so a manifest of a newer bridge
// returns *modules.NewerBridge and a newer vocabulary returns *NewerBlocks before any script is
// read. Files are opened through os.Root: a link that leads out of the folder is refused.
// Hidden folders (.git) are skipped; files other than the manifest and .js files are ignored.
func Load(dir string) (*Bundle, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, &BundleError{Message: err.Error()}
	}
	defer root.Close()
	fsys := root.FS()
	mdata, err := readRegular(root, modules.ManifestFile, maxManifestBytes)
	if err != nil {
		return nil, &BundleError{Path: modules.ManifestFile, Message: readMessage(err)}
	}
	m, err := modules.ParseManifest(mdata)
	if err != nil {
		return nil, err
	}
	if m.Blocks > modules.Version {
		return nil, &NewerBlocks{Blocks: m.Blocks}
	}
	b := &Bundle{Dir: dir, Manifest: m, Sources: map[string]string{}}
	total := 0
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return &BundleError{Path: p, Message: readMessage(err)}
		}
		if d.IsDir() {
			if p != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if path.Ext(p) != ".js" {
			return nil
		}
		if len(b.Sources) == MaxSourceFiles {
			return &BundleError{Message: fmt.Sprintf("more than %d .js files", MaxSourceFiles)}
		}
		if !d.Type().IsRegular() {
			return &BundleError{Path: p, Message: "not a regular file"}
		}
		data, err := readRegular(root, p, MaxSourceBytes-total)
		if errors.Is(err, errTooBig) {
			return &BundleError{Message: fmt.Sprintf(".js files over %d bytes in all", MaxSourceBytes)}
		}
		if err != nil {
			return &BundleError{Path: p, Message: readMessage(err)}
		}
		total += len(data)
		b.Sources[p] = string(data)
		return nil
	})
	if err != nil {
		var be *BundleError
		if errors.As(err, &be) {
			return nil, be
		}
		return nil, &BundleError{Message: err.Error()}
	}
	if _, ok := b.Sources[MainFile]; !ok {
		return nil, &BundleError{Path: MainFile, Message: "missing"}
	}
	return b, nil
}

// maxManifestBytes bounds manifest.toml.
const maxManifestBytes = 64 << 10

var (
	errTooBig     = errors.New("too large")
	errNotRegular = errors.New("not a regular file")
)

// readRegular reads a regular file of at most max bytes. It opens without blocking, so a FIFO
// or a device cannot hold Load, checks the type and size on the open file, and reads no more
// than max+1 bytes in case the file grows. Hard links cannot be told apart from files; the
// installer unpacks archives with no links or special files.
func readRegular(root *os.Root, name string, max int) ([]byte, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errNotRegular
	}
	if info.Size() > int64(max) {
		return nil, errTooBig
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, errTooBig
	}
	return data, nil
}

func readMessage(err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return "missing"
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
