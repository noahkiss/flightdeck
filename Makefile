GO ?= go
ZIG ?= zig

# libghostty-vt, pinned to the exact Ghostty commit that the go.mitchellh.com/libghostty
# version in go.mod was built against. Bump both together.
GHOSTTY_COMMIT := f54340ad77814fe319d136e5e915d6a48de8dd23
GHOSTTY_SRC := build/ghostty-src
GHOSTTY_PREFIX := $(CURDIR)/build/libghostty-vt
GHOSTTY_STAMP := $(GHOSTTY_PREFIX)/.built-$(GHOSTTY_COMMIT)
# Extra zig build flags for libghostty. CI and releases pass a fixed target and the baseline CPU
# (docs/specs/ci-release.md 7.3); empty builds for this machine. The stamp does not record them:
# run `make clean-libghostty` before building with other flags.
GHOSTTY_ZIGFLAGS ?=

# The ghostty build tag selects the cgo parser in internal/vt/ghostty.
GHOSTTY_ENV := PKG_CONFIG_PATH=$(GHOSTTY_PREFIX)/share/pkgconfig CGO_ENABLED=1

# The speech helper, gff-hab-speech (docs/specs/voice.md): sherpa-onnx's C API from its pinned
# static release, plus libopus from the system (pkg-config opus). Nothing of it is committed.
SHERPA_VERSION := 2.0.1
SHERPA_RELEASE := https://github.com/k2-fsa/sherpa-onnx/releases/download/v$(SHERPA_VERSION)
SHERPA_HEADER_URL := https://raw.githubusercontent.com/k2-fsa/sherpa-onnx/v$(SHERPA_VERSION)/sherpa-onnx/c-api/c-api.h
SHERPA_HEADER_SHA256 := 5698f38ba09ad2bdf085c56a6cbbd7741acf15e25f4ce5c7097b9702cadf437f
SHERPA_PREFIX := $(CURDIR)/build/sherpa-onnx
SHERPA_STAMP := $(SHERPA_PREFIX)/.built-$(SHERPA_VERSION)
SPEECH_OS := $(shell uname -s)
SPEECH_ARCH := $(shell uname -m)
ifeq ($(SPEECH_OS)-$(SPEECH_ARCH),Darwin-arm64)
SHERPA_ARCHIVE := sherpa-onnx-v$(SHERPA_VERSION)-osx-arm64-static-no-tts-lib
SHERPA_SHA256 := 69413605e7d0d1286d40fbf66e898f7abb53f350d6057cbd154ea2e3f1437d2a
else ifeq ($(SPEECH_OS)-$(SPEECH_ARCH),Darwin-x86_64)
SHERPA_ARCHIVE := sherpa-onnx-v$(SHERPA_VERSION)-osx-universal2-static-no-tts-lib
SHERPA_SHA256 := bd4cdc1c4122d249c46187fb36528733186f7bf52e8ceae716bd37255550a567
else ifeq ($(SPEECH_OS)-$(SPEECH_ARCH),Linux-x86_64)
SHERPA_ARCHIVE := sherpa-onnx-v$(SHERPA_VERSION)-linux-x64-static-no-tts-lib
SHERPA_SHA256 := 4b4c36e5560696e2864da3d9a7cdbb1fc1f0ce7af3509e19a659341debda009a
else ifeq ($(SPEECH_OS)-$(SPEECH_ARCH),Linux-aarch64)
SHERPA_ARCHIVE := sherpa-onnx-v$(SHERPA_VERSION)-linux-aarch64-static-lib
SHERPA_SHA256 := b6c6090e40fd18d6812f04fe21260602cd1223d7b5e83ad39dab87ad7c1c031d
endif
ifeq ($(SPEECH_OS),Darwin)
SHERPA_SYSLIBS := -lc++ -framework Foundation
SHA256SUM := shasum -a 256
else
SHERPA_SYSLIBS := -lstdc++ -lm -lpthread -ldl
SHA256SUM := sha256sum
endif
# On macOS the helper links libopus statically, through a pkg-config file of our own in
# build/opus-static that names the system's libopus.a. Under the hardened runtime, library
# validation refuses a library another team (or nobody) signed, so a helper signed into
# GFF Hab.app may load only system libraries; and the bundle then runs on Macs without libopus.
# The go build cache does not see a pkg-config file change, so a helper built before with a
# dynamic libopus would stay cached. CGO_LDFLAGS is part of the cache key: it carries
# -L<build/opus-static>, a folder that holds only opus.pc and so changes nothing in the link,
# as a marker that keeps static and dynamic builds apart.
ifeq ($(SPEECH_OS),Darwin)
OPUS_STATIC := $(CURDIR)/build/opus-static
SPEECH_OPUS := opus-static
SPEECH_LDFLAGS := CGO_LDFLAGS="-O2 -g -L$(OPUS_STATIC)"
endif
# The speech build tag selects cmd/gff-hab-speech; pkg-config finds sherpa-onnx and opus.
SPEECH_ENV := PKG_CONFIG_PATH=$(if $(OPUS_STATIC),$(OPUS_STATIC):)$(SHERPA_PREFIX)/lib/pkgconfig$(if $(PKG_CONFIG_PATH),:$(PKG_CONFIG_PATH)) $(SPEECH_LDFLAGS) CGO_ENABLED=1

.PHONY: gen test test-pure race fuzz check-gen check-pure release-schema libghostty build vet clean-libghostty \
	sherpa-onnx speech-helper test-speech clean-sherpa-onnx opus-static ci-full ci-fast release-build

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
		-Doptimize=ReleaseFast $(GHOSTTY_ZIGFLAGS) --prefix $(GHOSTTY_PREFIX)
	@touch $@

clean-libghostty:
	rm -rf $(GHOSTTY_SRC) $(GHOSTTY_PREFIX)

# Fetch sherpa-onnx's static C library and its C API header into build/ (ignored by git), check
# both checksums, and write a pkg-config file for them. Needs curl, tar with bzip2.
sherpa-onnx: $(SHERPA_STAMP)

$(SHERPA_STAMP):
	@test -n "$(SHERPA_ARCHIVE)" || { echo "no sherpa-onnx static library for $(SPEECH_OS) $(SPEECH_ARCH)"; exit 1; }
	@rm -rf $(SHERPA_PREFIX) && mkdir -p $(SHERPA_PREFIX)/include/sherpa-onnx/c-api $(SHERPA_PREFIX)/lib/pkgconfig
	curl -fsSL -o $(SHERPA_PREFIX)/lib.tar.bz2 $(SHERPA_RELEASE)/$(SHERPA_ARCHIVE).tar.bz2
	echo "$(SHERPA_SHA256)  $(SHERPA_PREFIX)/lib.tar.bz2" | $(SHA256SUM) -c -
	tar xjf $(SHERPA_PREFIX)/lib.tar.bz2 -C $(SHERPA_PREFIX)/lib --strip-components 2 '$(SHERPA_ARCHIVE)/lib'
	rm $(SHERPA_PREFIX)/lib.tar.bz2
	curl -fsSL -o $(SHERPA_PREFIX)/include/sherpa-onnx/c-api/c-api.h $(SHERPA_HEADER_URL)
	echo "$(SHERPA_HEADER_SHA256)  $(SHERPA_PREFIX)/include/sherpa-onnx/c-api/c-api.h" | $(SHA256SUM) -c -
	@libs=$$(cd $(SHERPA_PREFIX)/lib && ls lib*.a | sed 's/^lib\(.*\)\.a$$/-l\1/' | tr '\n' ' '); \
	if [ "$(SPEECH_OS)" = Linux ]; then libs="-Wl,--start-group $$libs -Wl,--end-group"; fi; \
	printf 'prefix=%s\nName: sherpa-onnx\nDescription: sherpa-onnx C API, static\nVersion: %s\nCflags: -I%s/include\nLibs: -L%s/lib %s %s\n' \
		"$(SHERPA_PREFIX)" "$(SHERPA_VERSION)" "$(SHERPA_PREFIX)" "$(SHERPA_PREFIX)" "$$libs" "$(SHERPA_SYSLIBS)" \
		> $(SHERPA_PREFIX)/lib/pkgconfig/sherpa-onnx.pc
	@touch $@

clean-sherpa-onnx:
	rm -rf $(SHERPA_PREFIX)

# macOS: write build/opus-static/opus.pc, which links the libopus.a beside the system's libopus.
opus-static:
	@test -n "$(OPUS_STATIC)" || { echo "opus-static is for macOS only: Linux links the system's libopus"; exit 1; }
	@libdir=$$(pkg-config --variable=libdir opus) && test -f "$$libdir/libopus.a" || \
		{ echo "no libopus.a in the folder pkg-config opus names; install libopus with its static library"; exit 1; }; \
	mkdir -p $(OPUS_STATIC) && \
	printf 'Name: opus\nDescription: libopus, linked statically\nVersion: %s\nCflags: %s\nLibs: %s/libopus.a\n' \
		"$$(pkg-config --modversion opus)" "$$(pkg-config --cflags opus)" "$$libdir" > $(OPUS_STATIC)/opus.pc

# Build the speech helper into build/gff-hab-speech. Install it beside gff-hab, or point
# $GFF_HAB_SPEECH_HELPER or [speech] helper in the hab's config file at it. On macOS,
# scripts/macos-bundle.sh puts it beside gff-hab in GFF Hab.app.
speech-helper: sherpa-onnx $(SPEECH_OPUS)
	$(SPEECH_ENV) $(GO) build -tags speech -o build/gff-hab-speech ./cmd/gff-hab-speech

# Run the helper's own tests (Opus decode; recognition when $GFF_SPEECH_MODEL_DIR names the model,
# with a clip from macOS say or the WAV $GFF_SPEECH_TEST_WAV names).
test-speech: sherpa-onnx $(SPEECH_OPUS)
	$(SPEECH_ENV) $(GO) test -tags speech ./cmd/gff-hab-speech

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

# Run the holder, structure, link, control, data stream, connection library, gff client core, pane size, command
# marks, peers, catalog, channels, harness, hook, git status, transcript, usage, service, push and speech tests three times under the race
# detector. No libghostty needed.
RACE_PKGS := ./internal/holder/... ./internal/holderproto/... ./internal/holderclient/... \
	./internal/holderreg/... ./internal/structure/... ./internal/link/... \
	./internal/control/... ./internal/datastream/... ./internal/habconn/... ./internal/hosts/... ./internal/merge/... ./internal/peers/... ./internal/catalog/... ./internal/channels/... ./internal/board/... ./internal/gffclient/... ./internal/panesize/... ./internal/termq/... \
	./internal/cmdmarks/... ./internal/readtools/... ./internal/confirm/... ./internal/suggest/... \
	./internal/harness/... ./internal/hook/... ./internal/opencode/... ./internal/gitstatus/... ./internal/worktree/... ./internal/transcript/... ./internal/usage/... \
	./internal/service/... ./internal/grants/... ./internal/push/... ./internal/settings/... ./internal/rounds/... \
	./internal/speech/... ./internal/speechproto/... ./internal/oggopus/... ./internal/attach/... \
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

# What .github/workflows/ci.yml runs after go-ci's gofmt and generate checks (docs/specs/ci-release.md
# 3.2): the full set on a pull request or a direct push, the fast set on the push that merges a
# pull request whose own run passed. Run one before pushing.
ci-full: check-pure vet test race

ci-fast: check-pure build

# Build the release binaries into RELEASE_DIR (docs/specs/ci-release.md 7.3): gff-hab and gff with
# libghostty linked in (-tags ghostty), gff-hab-holder in pure Go. release.yml passes the version
# from the tag, the platform's GHOSTTY_ZIGFLAGS and, on Linux, CC for the glibc floor.
RELEASE_VERSION ?= dev
RELEASE_BUILD ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
RELEASE_DIR ?= build/release
RELEASE_LDFLAGS = -s -w -X main.version=$(RELEASE_VERSION) -X main.build=$(RELEASE_BUILD)
# macOS: 13.0 is the floor libghostty builds for. The CGO flags carry it too, because the Go build
# cache does not key on MACOSX_DEPLOYMENT_TARGET and would link objects built for this Mac's macOS.
ifeq ($(SPEECH_OS),Darwin)
release-build: export MACOSX_DEPLOYMENT_TARGET = 13.0
release-build: export CGO_CFLAGS = -O2 -g -mmacosx-version-min=13.0
release-build: export CGO_LDFLAGS = -O2 -g -mmacosx-version-min=13.0
endif
release-build: libghostty
	@mkdir -p $(RELEASE_DIR)
	$(GHOSTTY_ENV) $(GO) build -tags ghostty -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(RELEASE_DIR)/ ./cmd/gff-hab ./cmd/gff
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(RELEASE_LDFLAGS)" -o $(RELEASE_DIR)/ ./cmd/gff-hab-holder

# Record the current schema as the released schema of its revision. Run at release time only.
release-schema:
	$(GO) run ./internal/apigen/cmd -root . -release
