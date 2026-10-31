package apigen

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
