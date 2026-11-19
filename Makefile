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

.PHONY: build test check install cross-check clean fixtures acceptance
build:
	$(GO) build $(LDFLAGS) -o $(BIN) ./cmd/easyhog

test:
	$(GO) test ./...

check:
	@files="$$(gofmt -l cmd internal)" && test -z "$$files" || { echo 'Go formatting needed'; exit 1; }
	$(GO) vet ./...

# Runs examples/consumers through a real Groundhog runner. Needs EASYHOG_GROUNDHOG_SH
# (a groundhog.sh with consumer-v1 and delivery-v1); EASYHOG_WAKE_CONSUMER is optional.
# Without them it skips with a message. Not part of `make test`.
fixtures:
	@bash examples/consumers/fixtures.sh

# The MVP acceptance session (internal/acceptance): a reader's whole session over
# temporary synthetic repositories, through the core and the TUI model, with real
# Groundhog runners on the pinned script. About 40 seconds. EASYHOG_ACCEPTANCE=1
# turns it on (plain `go test` skips it). EASYHOG_WAKE_CONSUMER, an absolute path
# to a wake adapter, adds the in-place wake on a private tmux server;
# unset (or without tmux), that step reports a skip and the rest runs. Needs bash.
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
