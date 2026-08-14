# devbay
#
# `make test` is the fast loop: unit tests only, no containers. `make check` is
# what CI runs and what a pull request should pass.

BIN     := bin/devbay
PKG     := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

.PHONY: all
all: build

.PHONY: build
build: ## build ./bin/devbay
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/devbay

.PHONY: install
install: ## install devbay into GOBIN
	go install -ldflags '$(LDFLAGS)' ./cmd/devbay

.PHONY: test
test: ## unit tests only -- no containers, seconds
	go test -short $(PKG)

.PHONY: test-all
test-all: ## the full suite, including Docker integration tests
	go test -timeout 20m $(PKG)

.PHONY: acceptance
acceptance: ## drive the real binary the way a developer does (see docs/ACCEPTANCE.md)
	# -count=1 because this suite builds the binary itself, so the test binary
	# does not depend on the packages under test and Go would happily serve a
	# cached pass after a change that broke the product.
	go test -tags acceptance -count=1 -timeout 30m -v ./internal/acceptance/

.PHONY: race
race: ## the full suite under the race detector
	go test -race -timeout 30m $(PKG)

.PHONY: vet
vet:
	go vet $(PKG)

.PHONY: fmt
fmt:
	gofmt -l -w $$(git ls-files '*.go')

.PHONY: fmt-check
fmt-check: ## fail if anything is unformatted
	@out=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$out" ]; then echo "not gofmt'd:"; echo "$$out"; exit 1; fi

.PHONY: tidy-check
tidy-check: ## fail if go.mod is not tidy
	@cp go.mod /tmp/devbay-go.mod.bak; cp go.sum /tmp/devbay-go.sum.bak; \
	go mod tidy; \
	if ! git diff --quiet -- go.mod go.sum; then \
		cp /tmp/devbay-go.mod.bak go.mod; cp /tmp/devbay-go.sum.bak go.sum; \
		echo "go.mod is not tidy; run: go mod tidy"; exit 1; \
	fi

.PHONY: claims-check
claims-check: ## fail if a number devbay publishes about itself is out of date
	@python3 scripts/claims.py

.PHONY: check
check: fmt-check vet tidy-check claims-check build test-all race ## everything CI runs

.PHONY: schema
schema: build ## print the manifest JSON Schema
	@$(BIN) schema

.PHONY: clean
clean:
	rm -rf bin dist

# Leaves nothing behind is a product promise, so make it checkable by hand too.
.PHONY: orphans
orphans: ## list any devbay-managed Docker resources still on this machine
	@docker ps -aq  --filter label=dev.devbay.managed | sed 's/^/container /'
	@docker volume  ls -q --filter label=dev.devbay.managed | sed 's/^/volume    /'
	@docker network ls -q --filter label=dev.devbay.managed | sed 's/^/network   /'

.PHONY: help
help:
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
