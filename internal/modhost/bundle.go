// Package modhost runs modules: it loads a module bundle (manifest.toml and main.js), runs it in
// its own goja runtime with the bridge of docs/specs/modules.md 7, under the limits of 8, and
// returns block trees checked by modules.ValidateTree. The module host process (modules.md 4,
// stage 5) holds one Module per enabled module; the backends the bridge reaches (the hab API,
// programs, HTTP, state storage) are injected. Pure Go. Spec: docs/specs/modules.md 5.1, 7.3, 8.4.
package modhost

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

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
	mdata, err := fs.ReadFile(fsys, modules.ManifestFile)
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
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return &BundleError{Path: p, Message: readMessage(err)}
		}
		total += len(data)
		if total > MaxSourceBytes {
			return &BundleError{Message: fmt.Sprintf(".js files over %d bytes in all", MaxSourceBytes)}
		}
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
