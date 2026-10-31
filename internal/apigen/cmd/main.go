// Command apigen writes the API schema, the Go client and the Swift package from package api.
// With -release it also copies the schema into api/released/<revision>.json.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/noahkiss/flightdeck/internal/apigen"
)

func main() {
	root := flag.String("root", ".", "repo root")
	release := flag.Bool("release", false, "also record the schema as the released schema of this revision")
	flag.Parse()

	files, err := apigen.Generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for path, data := range files {
		if err := write(filepath.Join(*root, path), data); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if *release {
		doc, _ := apigen.Build()
		path := filepath.Join(*root, apigen.ReleasedDir, fmt.Sprintf("%d.json", doc.Revision))
		if err := write(path, files[apigen.SchemaPath]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

// write skips files whose content is unchanged, so go generate keeps mtimes stable.
func write(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
