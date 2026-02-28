BIN      := lazycoding
PKG      := ./cmd/lazycoding/
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  := -ldflags "-s -w -X main.version=$(VERSION)"
DIST     := dist

.PHONY: build build-whisper test clean release \
        release-linux-amd64 release-linux-arm64 \
        release-darwin-amd64 release-darwin-arm64 \
        release-windows-amd64

# ── 本机构建 ─────────────────────────────────────────────────

## build: 为当前平台编译（不含语音识别）
build:
	go build $(LDFLAGS) -o $(BIN) $(PKG)

## build-whisper: 为当前平台编译（含 CGo whisper-native 语音识别）
##   前提：brew install whisper-cpp ffmpeg
build-whisper:
	go build -tags whisper $(LDFLAGS) -o $(BIN) $(PKG)

## test: 运行所有测试
test:
	go test ./...

## clean: 删除构建产物
clean:
	rm -f $(BIN) $(BIN).exe
	rm -rf $(DIST)

# ── 交叉编译发布包 ────────────────────────────────────────────
# 注意：CGo（-tags whisper）不支持交叉编译，发布包均不含 whisper-native。

## release: 为所有目标平台编译发布包
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

## help: 显示此帮助
help:
	@grep -E '^##' Makefile | sed 's/## /  /'
