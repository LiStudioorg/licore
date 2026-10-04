# Copyright (C) 2026 LiStudioorg
# SPDX-License-Identifier: AGPL-3.0-only
#
# LiCore 构建入口。设计要点：
#   - 全部目标都是纯 Go（CGO_ENABLED=0），产出静态二进制、无 glibc 依赖；
#   - 不再需要 C 工具链 / Android NDK：v0.8.0 起 `licore exec` 改为调用系统的
#     nsenter（util-linux / Toybox / busybox 均提供），因此没有 cgo 变体；
#   - `-tags nocgo_exec` 已废弃，保留兼容（不再有任何文件依赖该标签）；
#   - 不隐式下载工具链：缺什么就报错，绝不静默产出坏二进制。

BIN      := licore
DIST     := dist
GO       ?= go
# 版本号：make VERSION=0.8.0 all，或直接 -ldflags 覆盖。
VERSION  ?= 0.0.0-dev
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: all linux android darwin test vet fmt clean install help

# 默认目标：服务器三件套（linux amd64 + arm64 + android arm64）。
all: linux android
	@echo "构建完成，产物在 $(DIST)/"

## linux: 桌面 Linux（amd64 + arm64），纯 Go 静态二进制
linux:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-linux-amd64 .
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-linux-arm64 .
	@echo "  → $(DIST)/$(BIN)-linux-amd64, $(DIST)/$(BIN)-linux-arm64"

## android: Android ARM64，纯 Go（exec 依赖系统 nsenter：Toybox 自带，
##          旧版 Android 可装 busybox / Magisk）
android:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=android GOARCH=arm64 $(GO) build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-android-arm64 .
	@echo "  → $(DIST)/$(BIN)-android-arm64"
	@echo "     exec 需要 nsenter：Android 10+ 由 Toybox 自带；更早版本请装 busybox 或 Magisk。"

## darwin: macOS（在 VM 内运行，exec 由 VM 里的 nsenter 提供）
darwin:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-darwin-arm64 .
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-darwin-amd64 .
	@echo "  → $(DIST)/$(BIN)-darwin-arm64, $(DIST)/$(BIN)-darwin-amd64"

## test: 运行全部测试
test:
	CGO_ENABLED=0 $(GO) test ./... -count=1

## vet: 静态检查
vet:
	CGO_ENABLED=0 $(GO) vet ./...

## fmt: 格式检查（有未格式化文件则失败，不自动改写）
fmt:
	@out="$$(gofmt -l $$(git ls-files '*.go'))"; \
	if [ -n "$$out" ]; then echo "以下文件未格式化:"; echo "$$out"; exit 1; fi; \
	echo "gofmt 干净"

## clean: 清理构建产物
clean:
	rm -rf $(DIST)
	@echo "已清理 $(DIST)/"

## install: 装到 /usr/local/bin（默认装当前平台的 amd64 服务器版）
install:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-linux-amd64 .
	install -m 0755 $(DIST)/$(BIN)-linux-amd64 /usr/local/bin/$(BIN)
	@echo "已安装到 /usr/local/bin/$(BIN)"

## help: 显示本帮助
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
