BINARY := cloudconsole
DIST   := dist
PKG    := github.com/studio-ch/cloudconsole-cli

.PHONY: build
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(DIST)/$(BINARY) ./cmd/cloudconsole

.PHONY: test
test:
	go test ./... -race -count=1

.PHONY: lint
lint:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...

# Refresh the committed OpenAPI snapshot from the API source.
#
# Monorepo-only: this reaches into apps/api and does not work in the
# public mirror, which contains this module alone. The snapshot is
# committed, so the mirror needs neither this target nor the API source
# to build.
#
# --silent matters: pnpm prints a banner to stdout that would otherwise
# end up inside the JSON. The dump imports apps/api/src/app.ts directly
# and needs no database and no running server.
.PHONY: spec
spec:
	cd ../.. && pnpm --silent --filter @studio-cp/api openapi:dump > apps/cloudconsole-cli/api/openapi-3.0.json

# Regenerate the API client from the committed snapshot. oapi-codegen is
# pinned via the tool directive in go.mod, so this needs no global install.
.PHONY: generate
generate:
	go tool oapi-codegen -config api/oapi-codegen.yaml api/openapi-3.0.json
	gofmt -w internal/api/gen

# Verify the snapshot and the generated client are current. Run by CI;
# a failure means somebody changed the API without refreshing the client.
.PHONY: check-generate
check-generate: spec generate
	@git diff --exit-code -- api/openapi-3.0.json internal/api/gen || { \
		echo ""; \
		echo "The committed API client is out of date."; \
		echo "Run:  make -C apps/cloudconsole-cli spec generate"; \
		echo "and commit the result."; \
		exit 1; \
	}

.PHONY: snapshot
snapshot:
	goreleaser build --snapshot --clean --single-target

.PHONY: clean
clean:
	rm -rf $(DIST)
