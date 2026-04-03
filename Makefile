# STUN Max — 多平台交叉编译
# 使用: make help

.DEFAULT_GOAL := all

VERSION        ?= dev
BUILD_DIR      ?= build
LDFLAGS        := -s -w
WINDOWS_GUI_LD := $(LDFLAGS) -H windowsgui

.PHONY: all clean help prepare build-all
.PHONY: \
	server-linux-amd64 server-darwin-arm64 \
	client-gui-darwin client-gui-windows \
	client-cli-darwin client-cli-windows client-cli-linux \
	web-assets

# 动态帮助：所有带「## 说明」的目标会出现在 make help 中
help: ## 显示本帮助（所有可用目标与说明）
	@printf '%s\n' 'STUN Max 构建目标 (VERSION=$(VERSION), BUILD_DIR=$(BUILD_DIR)):' ''
	@grep -hE '^[a-zA-Z0-9_.-]+:.*?## ' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-32s\033[0m %s\n", $$1, $$2}'
	@printf '%s\n' '' '常用: make | make all    # 全量构建' '      make server-linux-amd64   # 仅 Linux 服务端' ''

all: build-all ## 清理并全量构建（默认目标）
	upx $(BUILD_DIR)/*linux*

clean: ## 删除 build 输出目录
	rm -rf "$(BUILD_DIR)"

prepare: | $(BUILD_DIR)

$(BUILD_DIR):
	mkdir -p "$(BUILD_DIR)"

build-all: clean prepare
	@echo "Building STUN Max $(VERSION) ..."
	@$(MAKE) -s server-linux-amd64
	@$(MAKE) -s server-darwin-arm64
	@$(MAKE) -s client-gui-darwin
	@$(MAKE) -s client-gui-windows
	@$(MAKE) -s client-cli-darwin
	@$(MAKE) -s client-cli-windows
	@$(MAKE) -s client-cli-linux
	@$(MAKE) -s web-assets
	@echo ""
	@echo "构建完成 (VERSION=$(VERSION)):"
	@ls -lh "$(BUILD_DIR)"/ | grep -v "^total" || true

# --- Server ---
server-linux-amd64: prepare ## 服务端 linux/amd64（CGO_ENABLED=0）
	@echo "  server (linux/amd64)"
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o "$(BUILD_DIR)/stun_max-server-linux-amd64" ./server/

server-darwin-arm64: prepare ## 服务端 darwin/arm64（CGO_ENABLED=0）
	@echo "  server (darwin/arm64)"
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o "$(BUILD_DIR)/stun_max-server-darwin-arm64" ./server/

# --- GUI Client ---
client-gui-darwin: prepare ## GUI 客户端 darwin/arm64
	@echo "  gui-client (darwin/arm64)"
	GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o "$(BUILD_DIR)/stun_max-client-darwin-arm64" ./client/

client-gui-windows: prepare ## GUI 客户端 windows/amd64（windowsgui）
	@echo "  gui-client (windows/amd64)"
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(WINDOWS_GUI_LD)" -o "$(BUILD_DIR)/stun_max-client-windows-amd64.exe" ./client/

# --- CLI Client ---
client-cli-darwin: prepare ## CLI 客户端 darwin/arm64（-tags cli）
	@echo "  cli-client (darwin/arm64)"
	GOOS=darwin GOARCH=arm64 go build -tags cli -ldflags="$(LDFLAGS)" -o "$(BUILD_DIR)/stun_max-cli-darwin-arm64" ./client/

client-cli-windows: prepare ## CLI 客户端 windows/amd64（-tags cli）
	@echo "  cli-client (windows/amd64)"
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags cli -ldflags="$(LDFLAGS)" -o "$(BUILD_DIR)/stun_max-cli-windows-amd64.exe" ./client/

client-cli-linux: prepare ## CLI 客户端 linux/amd64（-tags cli）
	@echo "  cli-client (linux/amd64)"
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags cli -ldflags="$(LDFLAGS)" -o "$(BUILD_DIR)/stun_max-cli-linux-amd64" ./client/

web-assets: prepare ## 复制嵌入用 web 静态资源到 build/web（与 server/web 同步）
	@echo "  web assets"
	cp -r server/web "$(BUILD_DIR)/web"
