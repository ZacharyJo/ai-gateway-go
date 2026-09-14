GOOS   ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
DIST_DIR := dist/$(GOOS)-$(GOARCH)
PROXY_BIN := $(DIST_DIR)/proxy
VERSION  ?= 0.1.0

.PHONY: build install clean test proxy-run proxy-start proxy-stop proxy-restart proxy-status proxy-logs

# 普通构建
build:
	mkdir -p $(DIST_DIR)
	go build -ldflags "-X ai-gateway-go/internal/proxy.Version=$(VERSION)" -o $(PROXY_BIN) .

# 安装到 ~/.ai-gateway/bin（可用 PREFIX 覆盖）
PREFIX ?= $(HOME)/.ai-gateway
install: build
	install -d $(PREFIX)/bin
	install -m 0755 $(PROXY_BIN) $(PREFIX)/bin/proxy
	@echo "Installed proxy -> $(PREFIX)/bin/proxy"

test:
	go test ./...

clean:
	rm -rf dist

# 前台运行
proxy-run:
	go run .

# 后台守护（复用 proxy 自带的 start/stop）
proxy-start: build
	$(PROXY_BIN) start
proxy-stop: build
	$(PROXY_BIN) stop
proxy-restart: build
	$(PROXY_BIN) restart
proxy-status: build
	$(PROXY_BIN) status
proxy-logs: build
	$(PROXY_BIN) logs
