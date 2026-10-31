# Repo layout

One Go module, `github.com/noahkiss/flightdeck`, at the repo root. Swift packages live under
`swift/`. The app project (Part 6) will sit beside them.

| Path | What it holds |
|---|---|
| `api/` | The gff-hab API as Go types: the source of truth for the schema |
| `api/schema.json` | Generated OpenRPC document; gff-hab serves it from `rpc.discover` |
| `api/client_gen.go` | Generated Go word constants, event decoding and client stubs |
| `api/released/<revision>.json` | The schema as released for each revision; the compat test compares against it |
| `internal/apigen/` | Schema builder, Go and Swift generators, compat checker |
| `swift/HabAPI/` | Swift package of generated `Codable` types for the app |
| `cmd/` | Binaries, as they land: `gff`, `gff-hab`, `gff-hab-holder` |
| `docs/specs/` | Behaviour specs |

Why one module: the binaries and the API ship together, and one `go.mod` keeps versions in step.
Split a module out only when something outside this repo must import it on its own schedule.

## The API schema

- Edit the Go types in `api/types.go` and the tables in `api/methods.go`. Never edit generated
  files.
- `make gen` (or `go generate ./...`) regenerates the schema, the Go client and the Swift package.
- `make test` runs the compat test. It fails when the schema breaks the released schema of the same
  revision (spec 3.3). A breaking change needs a new revision and its translator.
- `make release-schema` records the current schema in `api/released/`. Run it when a revision ships.
  Until the first release, `1.json` is a baseline that may be rewritten on purpose.

Rules the generator follows:

- A pointer field, or a field tagged `omitempty`, is optional. Every other field is required.
- A named string type listed in `api.Enums` is an open word set. Swift decodes unknown words.
  Adding a word to an input set is compatible; adding one to an output set is breaking.
- A required array must go out as `[]`, never `null`. Go marshals a nil slice as `null`, so the
  server must make every required slice non-nil.
