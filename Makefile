SHELL := /bin/bash
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DEMO ?= 0
RUN_PORT ?= 8080
RUN_STATE_DIR ?= $(CURDIR)/.run-state

.PHONY: all deps ui server build run release demo demo-serve clean typegen e2e

all: build

# --- Dependencies (install once) ---
UI_DIR := ui

deps: ## Install UI dependencies
	@echo "==> Installing UI dependencies (flutter pub get)"
	cd $(UI_DIR) && flutter pub get

# --- Build steps ---
ui: deps ## Build UI to ./web
	@echo "==> Building UI (Flutter with WASM)"
	cd $(UI_DIR) && flutter build web --wasm --release --base-href "/"
	@echo "==> Copying artifacts to ./web"
	rm -rf web/*
	mkdir -p web
	cp -r $(UI_DIR)/build/web/* web/
	mv web/index.html web/entry.html

server: ## Build piccolod with embedded ./web
	@echo "==> Building piccolod (version=$(VERSION))"
	go build -ldflags "-X main.version=$(VERSION)" -o piccolod ./cmd/piccolod

server-release: ## Build piccolod with embedded ./web
	@echo "==> Building release piccolod (version=$(VERSION))"
	go build -buildmode=pie -ldflags "-s -w -X main.version=$(VERSION)" -o piccolod ./cmd/piccolod

build: ui server
	@echo "==> Build complete: ./piccolod with embedded ./web"

build-release: ui server-release
	@echo "==> Build complete: ./piccolod with embedded ./web"

# --- Run targets ---
run: build ## Build (non-demo) and run piccolod locally
	@echo "==> Running piccolod on http://localhost:$(RUN_PORT) (state dir: $(RUN_STATE_DIR))"
	mkdir -p "$(RUN_STATE_DIR)"
	PORT=$(RUN_PORT) PICCOLO_STATE_DIR="$(RUN_STATE_DIR)" ./piccolod

run-fresh: build ## Build and run piccolod with a temporary state dir
	@echo "==> Running piccolod on http://localhost:$(RUN_PORT) with a fresh ephemeral state dir"
	@set -euo pipefail; tmpdir="$$(mktemp -d)"; \
	  echo "   state dir $$tmpdir"; \
	  cleanup() { sleep 2; rm -rf "$$tmpdir"; }; \
	  trap cleanup EXIT; \
	  PORT=$(RUN_PORT) PICCOLO_STATE_DIR="$$tmpdir" ./piccolod

release: clean deps typegen ## Produce a clean release build (non-demo)
	$(MAKE) build DEMO=0
	@echo "==> Release build available at ./piccolod"

# --- Utilities ---
typegen: ## Regenerate API types (not required yet)
	@true

clean:
	rm -rf web/* piccolod
	rm -rf .run-state .e2e-state
	cd $(UI_DIR) && flutter clean

e2e:
	@echo "E2E tests not yet ported to Flutter"

# Removed legacy demo and separate real config; unified on single config.
