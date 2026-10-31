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
| `internal/vt/` | The terminal parser interface gff-hab keeps per pane, and `Fake`, a pure-Go stand-in for tests |
| `internal/vt/ghostty/` | The libghostty-vt parser through cgo. Builds only with the `ghostty` build tag |
| `internal/vt/vttest/` | The shared parser tests and their hand-written fixture |
| `build/` | Ignored by git. `make libghostty` puts the Ghostty source and the built library here |
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

## The terminal parser (libghostty)

gff-hab parses pane output with libghostty-vt, Ghostty's terminal library, through cgo.

- **Pinned, not vendored.** `go.mod` pins the Go bindings, `go.mitchellh.com/libghostty`, to one
  commit. The Makefile pins the Ghostty commit those bindings were built against
  (`GHOSTTY_COMMIT`). Bump both together. No Ghostty source is committed here.
- **Zig 0.17.0** builds libghostty-vt (pinned in `mise.toml`; the Ghostty commit's
  `minimum_zig_version`). `make libghostty` fetches the pinned commit into `build/ghostty-src` and
  runs `zig build -Demit-lib-vt` into `build/libghostty-vt`. The first build takes a few minutes;
  later builds are skipped by a stamp file. CMake is not needed.
- **The `ghostty` build tag.** Only `internal/vt/ghostty` uses cgo, and only with `-tags ghostty`.
  Without the tag, `go build ./...` and `go test ./...` skip that package and work on any machine,
  with or without the library. `make build`, `make vet` and `make test` build the library first,
  then pass the tag and `PKG_CONFIG_PATH`. Linking is static, so binaries need no shared library.
- **cgo stays in one package.** Every other package, the holder included, must build with
  `CGO_ENABLED=0`. `make check-pure` proves it.
- **Snapshot encodings.** `snapshot-vt` (the baseline, spec Q6) is libghostty's VT formatter output
  with scrollback and modes. `snapshot-ghostty` is libghostty's own snapshot format. The fake
  supports `snapshot-vt` only.
- **Golden files.** `go test ./internal/vt/... -update` rewrites `vttest/testdata/fixture.golden`.
  The fake and libghostty must produce the same dump.
- **Licences.** Ghostty and the bindings are MIT. `NOTICE` carries their notices for binaries.
