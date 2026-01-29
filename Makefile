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
	sudo -E PORT=$(RUN_PORT) PICCOLO_STATE_DIR="$(RUN_STATE_DIR)" ./piccolod

run-fresh: build ## Build and run piccolod with a temporary state dir
	@echo "==> Running piccolod on http://localhost:$(RUN_PORT) with a fresh ephemeral state dir"
	@set -euo pipefail; tmpdir="$$(mktemp -d)"; \
	  echo "   state dir $$tmpdir"; \
	  cleanup() { sleep 2; rm -rf "$$tmpdir"; }; \
	  trap cleanup EXIT; \
	  sudo -E PORT=$(RUN_PORT) PICCOLO_STATE_DIR="$$tmpdir" ./piccolod

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

service: ## Build and install/update piccolod systemd service
	@if [ ! -f piccolod ]; then \
		echo "==> piccolod binary not found, building..."; \
		$(MAKE) build; \
	else \
		echo "==> Using existing piccolod binary"; \
	fi
	@echo "==> Generating systemd service file (PORT=$(RUN_PORT))..."
	@echo '[Unit]' > piccolod.service
	@echo 'Description=Piccolo Daemon' >> piccolod.service
	@echo 'After=network.target' >> piccolod.service
	@echo '' >> piccolod.service
	@echo '[Service]' >> piccolod.service
	@echo 'Type=notify' >> piccolod.service
	@echo 'User=root' >> piccolod.service
	@echo 'Group=root' >> piccolod.service
	@echo 'ExecStart=/usr/local/bin/piccolod' >> piccolod.service
	@echo 'Environment="PORT=$(RUN_PORT)"' >> piccolod.service
	@echo 'Environment="PICCOLO_STATE_DIR=/var/lib/piccolod"' >> piccolod.service
	@echo 'Restart=always' >> piccolod.service
	@echo 'RestartSec=5' >> piccolod.service
	@echo 'TimeoutStopSec=120' >> piccolod.service
	@echo 'KillMode=mixed' >> piccolod.service
	@echo '' >> piccolod.service
	@echo '[Install]' >> piccolod.service
	@echo 'WantedBy=multi-user.target' >> piccolod.service
	@echo "==> Installing/Updating piccolod service"
	@if systemctl is-active --quiet piccolod; then \
		echo "Stopping running service to update binary..."; \
		sudo systemctl stop piccolod; \
	fi
	sudo cp piccolod /usr/local/bin/piccolod
	sudo cp piccolod.service /etc/systemd/system/piccolod.service
	sudo systemctl daemon-reload
	sudo systemctl enable piccolod
	sudo systemctl start piccolod
	@echo "==> Service piccolod is now running:"
	@systemctl status piccolod --no-pager
	@rm piccolod.service

service-uninstall: ## Uninstall piccolod systemd service
	@echo "==> Uninstalling piccolod service"
	@if systemctl is-active --quiet piccolod; then \
		echo "Stopping service..."; \
		sudo systemctl stop piccolod; \
	fi
	sudo systemctl disable piccolod || true
	sudo rm -f /etc/systemd/system/piccolod.service
	sudo rm -f /usr/local/bin/piccolod
	sudo systemctl daemon-reload
	@echo "==> Service piccolod uninstalled"

# Removed legacy demo and separate real config; unified on single config.
