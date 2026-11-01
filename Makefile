GO ?= go
ZIG ?= zig

# libghostty-vt, pinned to the exact Ghostty commit that the go.mitchellh.com/libghostty
# version in go.mod was built against. Bump both together.
GHOSTTY_COMMIT := f54340ad77814fe319d136e5e915d6a48de8dd23
GHOSTTY_SRC := build/ghostty-src
GHOSTTY_PREFIX := $(CURDIR)/build/libghostty-vt
GHOSTTY_STAMP := $(GHOSTTY_PREFIX)/.built-$(GHOSTTY_COMMIT)

# The ghostty build tag selects the cgo parser in internal/vt/ghostty.
GHOSTTY_ENV := PKG_CONFIG_PATH=$(GHOSTTY_PREFIX)/share/pkgconfig CGO_ENABLED=1

.PHONY: gen test test-pure race fuzz check-gen check-pure release-schema libghostty build vet clean-libghostty

# Regenerate api/schema.json, api/client_gen.go and the Swift package.
gen:
	$(GO) generate ./...

# Build libghostty-vt into build/ (ignored by git). Needs zig (mise.toml) and git.
libghostty: $(GHOSTTY_STAMP)

$(GHOSTTY_STAMP):
	@mkdir -p $(GHOSTTY_SRC)
	@if [ ! -d $(GHOSTTY_SRC)/.git ]; then git -C $(GHOSTTY_SRC) init -q && \
		git -C $(GHOSTTY_SRC) remote add origin https://github.com/ghostty-org/ghostty.git; fi
	git -C $(GHOSTTY_SRC) fetch -q --depth 1 origin $(GHOSTTY_COMMIT)
	git -C $(GHOSTTY_SRC) checkout -q --detach $(GHOSTTY_COMMIT)
	cd $(GHOSTTY_SRC) && $(ZIG) build -Demit-lib-vt -Demit-xcframework=false \
		-Doptimize=ReleaseFast --prefix $(GHOSTTY_PREFIX)
	@touch $@

clean-libghostty:
	rm -rf $(GHOSTTY_SRC) $(GHOSTTY_PREFIX)

# Build every package, the cgo parser included.
build: libghostty
	$(GHOSTTY_ENV) $(GO) build -tags ghostty ./...

vet: libghostty
	$(GHOSTTY_ENV) $(GO) vet -tags ghostty ./...

# Run every Go test, the libghostty tests included.
test: libghostty
	$(GHOSTTY_ENV) $(GO) test -tags ghostty ./...

# Run the tests that need no C toolchain and no libghostty.
test-pure:
	CGO_ENABLED=0 $(GO) test ./...

# Run the holder, structure, link, control, data stream, pane size, command marks, harness, hook,
# git status, transcript and service tests three times under the race detector. No libghostty needed.
RACE_PKGS := ./internal/holder/... ./internal/holderproto/... ./internal/holderclient/... \
	./internal/holderreg/... ./internal/structure/... ./internal/link/... \
	./internal/control/... ./internal/datastream/... ./internal/panesize/... ./internal/termq/... \
	./internal/cmdmarks/... \
	./internal/harness/... ./internal/hook/... ./internal/gitstatus/... ./internal/transcript/... \
	./internal/service/... \
	./cmd/...
race:
	$(GO) test -race -count=3 $(RACE_PKGS)

# Run each fuzz target in internal/holderproto for FUZZTIME, one after the other. For nightly use.
FUZZTIME ?= 30s
FUZZ_PKG := ./internal/holderproto
fuzz:
	@set -e; for f in $$($(GO) test -list '^Fuzz' $(FUZZ_PKG) | grep '^Fuzz'); do \
		echo "fuzz $$f for $(FUZZTIME)"; \
		$(GO) test -run '^$$' -fuzz "^$$f$$" -fuzztime $(FUZZTIME) $(FUZZ_PKG); \
	done

# Prove that every package except internal/vt/ghostty builds without cgo.
check-pure:
	CGO_ENABLED=0 $(GO) build ./...
	CGO_ENABLED=0 $(GO) vet ./...

# Fail when a generated file is stale. CI runs this.
check-gen: gen
	git diff --exit-code -- api swift

# Record the current schema as the released schema of its revision. Run at release time only.
release-schema:
	$(GO) run ./internal/apigen/cmd -root . -release
