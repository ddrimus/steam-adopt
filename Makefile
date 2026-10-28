# Build, test, lint, release and install targets for steam-adopt
BINARY  := steam-adopt
PKG     := ./cmd/steam-adopt
# The release tag without its v, like the release workflow passes it
VERSION ?= $(or $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null)),dev)
PREFIX  ?= $(HOME)/.local
LDFLAGS := -s -w -X main.version=$(VERSION)
# Systems released: tar.gz for Linux and macOS, zip for Windows
TARGETS := linux/amd64 darwin/amd64 windows/amd64
# Analysis tools, pinned so every run checks the same way, installed into bin/tools
TOOLS       := $(CURDIR)/bin/tools
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0

# Recipes fail on the first error, even inside pipes
SHELL := bash
.SHELLFLAGS := -euo pipefail -c

# Static, reproducible builds that run on any Linux, macOS or Windows
export CGO_ENABLED := 0
GOBUILD := go build -trimpath -ldflags "$(LDFLAGS)"

.PHONY: all build release cross test cover lint vuln fmt install uninstall clean

all: lint cross test build

build:
	$(GOBUILD) -o bin/$(BINARY) $(PKG)

# Deterministic archives: sorted names, fixed dates and owners, and no gzip timestamp
release:
	rm -rf release && mkdir -p release
	for target in $(TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; name=$(BINARY)-$(VERSION)-$$os-$$arch; ext=; \
		[[ $$os == windows ]] && ext=.exe; \
		mkdir -p release/$$name; \
		GOOS=$$os GOARCH=$$arch $(GOBUILD) -o release/$$name/$(BINARY)$$ext $(PKG); \
		cp README.md LICENSE release/$$name/; \
		if [[ $$os == windows ]]; then \
			find release/$$name -exec touch -d '2020-01-01 00:00:00 UTC' {} +; \
			(cd release && find $$name | LC_ALL=C sort | TZ=UTC zip -X -9 -q $$name.zip -@); \
		else \
			tar -C release --sort=name --mtime='UTC 2020-01-01' --owner=0 --group=0 --numeric-owner -cf - $$name | gzip -n -9 > release/$$name.tar.gz; \
		fi; \
		rm -rf release/$$name; \
	done

# Every system's build and tests compile, which the Linux runner can check without the others
cross:
	for target in $(TARGETS); do GOOS=$${target%/*} GOARCH=$${target#*/} go vet ./...; done

# The race detector needs cgo
test:
	CGO_ENABLED=1 go test -race -cover ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

# gofmt, go vet and staticcheck, the last for every released system since each has code of its own
lint:
	@test -z "$$(gofmt -l .)" || { echo "Not gofmt'ed:"; gofmt -l .; exit 1; }
	go vet ./...
	GOBIN="$(TOOLS)" go install $(STATICCHECK)
	for target in $(TARGETS); do GOOS=$${target%/*} GOARCH=$${target#*/} "$(TOOLS)/staticcheck" ./...; done

# Known vulnerabilities in the code and the Go version it's built with, needs the network for the database
vuln:
	GOBIN="$(TOOLS)" go install $(GOVULNCHECK)
	"$(TOOLS)/govulncheck" ./...

fmt:
	gofmt -w .

install: build
	install -Dm755 bin/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/$(BINARY)

clean:
	rm -rf bin release coverage.out
