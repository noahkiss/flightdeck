package apigen

import "fmt"

// Paths of the generated files, relative to the repo root.
const (
	SchemaPath   = "api/schema.json"
	GoClientPath = "api/client_gen.go"
	SwiftPath    = "swift/HabAPI/Sources/HabAPI/HabAPI.swift"
	ReleasedDir  = "api/released"
)

// Generate returns every generated file, keyed by its path relative to the repo root.
func Generate() (map[string][]byte, error) {
	doc, err := Build()
	if err != nil {
		return nil, err
	}
	if err := checkSwiftNames(doc); err != nil {
		return nil, err
	}
	goSrc, err := GoClient()
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		SchemaPath:   doc.JSON(),
		GoClientPath: goSrc,
		SwiftPath:    Swift(doc),
	}, nil
}

// checkSwiftNames refuses a field whose Swift name is self. Backticks escape every other keyword,
// but a property named self hides the instance in the generated init, so the package no longer
// compiles.
func checkSwiftNames(d *Document) error {
	for _, name := range sortedKeys(d.Components.Schemas) {
		for _, pn := range sortedKeys(d.Components.Schemas[name].Properties) {
			if id := camel(pn, false); id == "self" || id == "Self" {
				return fmt.Errorf("apigen: %s.%s: a field named %q cannot be used in Swift; rename it", name, pn, pn)
			}
		}
	}
	return nil
}
