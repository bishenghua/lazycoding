BIN      := lazycoding
PKG      := ./cmd/lazycoding/
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -ldflags "-s -w -X main.version=$(VERSION)"
DIST     := dist

.PHONY: build build-whisper test clean release \
        release-linux-amd64 release-linux-arm64 \
        release-darwin-amd64 release-darwin-arm64 \
        release-windows-amd64

# ── Local builds ─────────────────────────────────────────────

## build: compile for the current platform (no voice recognition)
build:
	go build $(LDFLAGS) -o $(BIN) $(PKG)

## build-whisper: compile with embedded CGo whisper-native voice recognition
##   prerequisite: brew install whisper-cpp ffmpeg
build-whisper:
	go build -tags whisper $(LDFLAGS) -o $(BIN) $(PKG)

## test: run all tests
test:
	go test ./...

## clean: remove build artefacts
clean:
	rm -f $(BIN) $(BIN).exe
	rm -rf $(DIST)

# ── Cross-compiled release binaries ──────────────────────────
# Note: CGo (-tags whisper) does not support cross-compilation.
# Release binaries are built without whisper-native.

## release: build release binaries for all target platforms
release: release-linux-amd64 release-linux-arm64 \
         release-darwin-amd64 release-darwin-arm64 \
         release-windows-amd64

release-linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
	go build $(LDFLAGS) -o $(DIST)/$(BIN)-linux-amd64 $(PKG)

release-linux-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
	go build $(LDFLAGS) -o $(DIST)/$(BIN)-linux-arm64 $(PKG)

release-darwin-amd64:
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 \
	go build $(LDFLAGS) -o $(DIST)/$(BIN)-darwin-amd64 $(PKG)

release-darwin-arm64:
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 \
	go build $(LDFLAGS) -o $(DIST)/$(BIN)-darwin-arm64 $(PKG)

release-windows-amd64:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
	go build $(LDFLAGS) -o $(DIST)/$(BIN)-windows-amd64.exe $(PKG)

## help: show available targets
help:
	@grep -E '^##' Makefile | sed 's/## /  /'
