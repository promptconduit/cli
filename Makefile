.PHONY: build test clean install release snapshot lint

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
