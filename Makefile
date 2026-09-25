BINARY  := cubcoder
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# release builds drop --dirty: the dist binaries are tracked, so rebuilding
# them dirties the tree, which would otherwise taint the embedded version.
RELVERSION := $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"
GOFILES := $(shell find . -name '*.go')

SHA256 := $(shell command -v sha256sum >/dev/null 2>&1 && echo "sha256sum" || echo "shasum -a 256")

.PHONY: build vet test cross dist release install clean

## build: compile for the host platform
build:
	$(GOBUILD) -o $(BINARY) .

## vet: static checks
vet:
	go vet ./...

## test: run tests
test:
	go test ./...

## cross: build binaries for common targets into dist/
cross: dist/$(BINARY)-linux-amd64 dist/$(BINARY)-linux-arm64 dist/$(BINARY)-darwin-arm64 dist/$(BINARY)-darwin-amd64

dist/$(BINARY)-linux-amd64: $(GOFILES)
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $@ .
dist/$(BINARY)-linux-arm64: $(GOFILES)
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $@ .
dist/$(BINARY)-darwin-arm64: $(GOFILES)
	GOOS=darwin GOARCH=arm64 $(GOBUILD) -o $@ .
dist/$(BINARY)-darwin-amd64: $(GOFILES)
	GOOS=darwin GOARCH=amd64 $(GOBUILD) -o $@ .

## dist: the binary client VMs run (linux/amd64)
dist: dist/$(BINARY)-linux-amd64

## release: cross-build all targets and write checksums clients can verify
release:
	rm -f dist/$(BINARY)-*
	$(MAKE) cross VERSION=$(RELVERSION)
	cd dist && $(SHA256) $(BINARY)-* > SHA256SUMS.txt
	@echo "built $(VERSION):"
	@cat dist/SHA256SUMS.txt

## install: build and install to /usr/local/bin (needs sudo)
install: build
	install -m 0755 $(BINARY) /usr/local/bin/$(BINARY)

## clean: remove build artifacts
clean:
	rm -f $(BINARY)
	rm -rf dist
