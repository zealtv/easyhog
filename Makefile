GO ?= go
PREFIX ?= $(HOME)/.local
DESTDIR ?=
BIN := bin/easyhog
# The release version, from VERSION at the root, stamped in as vX.Y.Z (what `go install` reports).
VERSION := $(shell cat VERSION 2>/dev/null)
LDFLAGS := $(if $(VERSION),-ldflags "-X main.version=v$(VERSION)")
export CGO_ENABLED = 0
# Never reuse another worktree's module or build cache.
export GOPATH := $(CURDIR)/.cache/go
export GOMODCACHE := $(CURDIR)/.cache/go/pkg/mod
export GOCACHE := $(CURDIR)/.cache/go-build
# Keep the module cache writable, so a checkout can be removed with plain rm -rf.
GOFLAGS += -modcacherw
export GOFLAGS

.PHONY: build test check install cross-check clean acceptance
build:
	$(GO) build $(LDFLAGS) -o $(BIN) ./cmd/easyhog

test:
	$(GO) test ./...

check:
	@files="$$(gofmt -l cmd internal)" && test -z "$$files" || { echo 'Go formatting needed'; exit 1; }
	$(GO) vet ./...

# The acceptance session (internal/acceptance): a reader's whole session over
# temporary synthetic repositories, through the core and the TUI model, with real
# ticks of the pinned Groundhog script and the example runs in examples/runs.
# Under a minute. EASYHOG_ACCEPTANCE=1 turns it on (plain `go test` skips it).
# Needs bash.
acceptance:
	EASYHOG_ACCEPTANCE=1 $(GO) test -count=1 -timeout 10m -v ./internal/acceptance

install: build
	install -d "$(DESTDIR)$(PREFIX)/bin"
	install -m 755 "$(BIN)" "$(DESTDIR)$(PREFIX)/bin/easyhog"

cross-check:
	@set -e; for os in darwin linux; do \
	  for arch in amd64 arm64; do \
	    echo "Building $$os/$$arch (CGO_ENABLED=0)"; \
	    GOOS=$$os GOARCH=$$arch $(GO) build $(LDFLAGS) -o "dist/easyhog-$$os-$$arch" ./cmd/easyhog; \
	  done; \
	done

clean:
	rm -rf bin dist
