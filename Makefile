GO ?= go

.PHONY: gen test check-gen release-schema

# Regenerate api/schema.json, api/client_gen.go and the Swift package.
gen:
	$(GO) generate ./...

test:
	$(GO) test ./...

# Fail when a generated file is stale. CI runs this.
check-gen: gen
	git diff --exit-code -- api swift

# Record the current schema as the released schema of its revision. Run at release time only.
release-schema:
	$(GO) run ./internal/apigen/cmd -root . -release
