.PHONY: build test clean install release snapshot lint refresh-extension refresh-graph

# Build configuration
BINARY_NAME := promptconduit
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -s -w -X github.com/promptconduit/cli/cmd.Version=$(VERSION)

# Go commands
GOCMD := go
GOBUILD := $(GOCMD) build
GOTEST := $(GOCMD) test
GOCLEAN := $(GOCMD) clean
GOMOD := $(GOCMD) mod

# Build the binary
build:
	$(GOBUILD) -ldflags "$(LDFLAGS)" -o $(BINARY_NAME) .

# Build for all platforms
build-all:
	GOOS=darwin GOARCH=amd64 $(GOBUILD) -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)_darwin_amd64 .
	GOOS=darwin GOARCH=arm64 $(GOBUILD) -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)_darwin_arm64 .
	GOOS=linux GOARCH=amd64 $(GOBUILD) -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)_linux_amd64 .
	GOOS=linux GOARCH=arm64 $(GOBUILD) -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)_linux_arm64 .
	GOOS=windows GOARCH=amd64 $(GOBUILD) -ldflags "$(LDFLAGS)" -o dist/$(BINARY_NAME)_windows_amd64.exe .

# Run tests
test:
	$(GOTEST) -v ./...

# Clean build artifacts
clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
	rm -rf dist/

# Install locally
install: build
	cp $(BINARY_NAME) /usr/local/bin/

# Run go mod tidy
tidy:
	$(GOMOD) tidy

# Lint the code (requires golangci-lint)
lint:
	golangci-lint run

# Create a snapshot release with GoReleaser
snapshot:
	goreleaser release --snapshot --clean

# Create a release (requires GITHUB_TOKEN)
release:
	goreleaser release --clean

# Dev: build and install hooks for testing
dev: build
	./$(BINARY_NAME) install claude-code

# Show version
version:
	@echo $(VERSION)

# Path to the editor-extension repo (sibling checkout by default) and the
# embedded .vsix the CLI sideloads via `install cursor`.
EXTENSION_DIR ?= ../editor-extension
EXTENSION_VSIX := internal/extension/embedded/promptconduit-cost.vsix

# Rebuild the bundled cost extension .vsix from the editor-extension repo and
# drop it into internal/extension/dist/ for go:embed. Run this after the
# extension changes so the CLI ships the matching build. Requires npm and the
# editor-extension checkout at $(EXTENSION_DIR).
refresh-extension:
	cd $(EXTENSION_DIR) && npm ci && npm run compile && \
		npx --yes @vscode/vsce package --out "$(CURDIR)/$(EXTENSION_VSIX)"
	@echo "Refreshed $(EXTENSION_VSIX)"

# The self-contained Session Graph page `promptconduit graph` serves, built from
# the editor-extension's PORTABLE graph core (same code the extension renders).
GRAPH_HTML := internal/graph/ui/graph.html

# Rebuild the embedded graph page from the editor-extension repo. esbuild output
# is deterministic, so this is byte-stable — the refresh workflow can diff it.
# Run after the graph's TypeScript (sessionTree/render/mount/styles) changes.
refresh-graph:
	cd $(EXTENSION_DIR) && npm ci && \
		node dev/build-cli-bundle.mjs --out "$(CURDIR)/$(GRAPH_HTML)"
	@echo "Refreshed $(GRAPH_HTML)"
