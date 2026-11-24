# Nova 面板构建

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_TIME ?= $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.buildTime=$(BUILD_TIME)

DIST := ../dist
PANEL := ./cmd/nova-panel

.PHONY: all build linux-amd64 linux-arm64 dev test clean checksums

all: linux-amd64 linux-arm64 checksums

## linux-amd64: 交叉编译 x86_64 Linux 版本
linux-amd64:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
		-trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/nova-panel-linux-amd64 $(PANEL)
	@echo "✓ $(DIST)/nova-panel-linux-amd64"

## linux-arm64: 交叉编译 ARM64 Linux 版本
linux-arm64:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
		-trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/nova-panel-linux-arm64 $(PANEL)
	@echo "✓ $(DIST)/nova-panel-linux-arm64"

## dev: 本地编译（当前平台，用于开发调试）
dev:
	go build -ldflags "$(LDFLAGS)" -o /tmp/nova-panel $(PANEL)

## test: 跑测试
test:
	go vet ./...
	go test ./... 2>/dev/null || true

## checksums: 生成 SHA256 清单
checksums:
	@cd $(DIST) && sha256sum nova-panel-linux-* > SHA256SUMS 2>/dev/null || \
	 shasum -a 256 nova-panel-linux-* > SHA256SUMS
	@cat $(DIST)/SHA256SUMS

## clean: 清理构建产物
clean:
	rm -rf $(DIST)
	go clean

## help: 显示帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
