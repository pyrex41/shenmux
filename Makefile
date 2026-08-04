SHELL := /bin/bash
BIN_DIR ?= bin

.PHONY: all build web-build workspace-component-build workspace-component-test workspace-runtime-test workspace-test golem-workspace-build test test-relay test-deploy race vet guards guard-check audit shen bifrost check clean install

all: check build

web-build:
	npm run build --prefix web

workspace-component-build:
	cargo build --manifest-path runtime/workspace-component/Cargo.toml --target wasm32-wasip2 --release
	node web/node_modules/@bytecodealliance/jco/src/jco.js transpile runtime/workspace-component/target/wasm32-wasip2/release/shenmux_workspace_component.wasm --out-dir runtime/workspace-component/generated --name workspace_component

workspace-component-test: workspace-component-build web-build
	node scripts/test-workspace-component.mjs

workspace-runtime-test:
	node scripts/test-workspace-runtime.mjs

workspace-test: workspace-component-test workspace-runtime-test

golem-workspace-build:
	command -v golem >/dev/null || (echo "golem CLI is required for the experimental workspace agent" >&2; exit 1)
	cd runtime/golem-workspace && golem build --yes

build: workspace-component-build web-build
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/shenmux ./cmd/shenmux
	go build -o $(BIN_DIR)/muxd ./cmd/muxd
	go build -o $(BIN_DIR)/muxctl ./cmd/muxctl
	go build -o $(BIN_DIR)/shenmux-web ./cmd/shenmux-web

test:
	go test ./...

test-relay:
	go test ./internal/relay ./internal/policy ./internal/appstate ./internal/transport ./internal/update ./cmd/shenmux

test-deploy:
	CGO_ENABLED=0 go build ./cmd/shenmux ./cmd/muxd ./cmd/muxctl ./cmd/shenmux-web

race:
	go test -race ./...

vet:
	go vet ./...

guards:
	./scripts/shengen-codegen.sh

guard-check:
	./scripts/shengen-codegen.sh --check

audit:
	./scripts/shenguard-audit.sh

shen:
	./scripts/shen-check.sh

bifrost:
	./scripts/run-bifrost.sh

check: web-build guard-check audit shen test vet
	go build ./...
	CGO_ENABLED=0 go build ./...

install: build
	install -m 0755 $(BIN_DIR)/shenmux $(HOME)/.local/bin/shenmux
	install -m 0755 $(BIN_DIR)/muxd $(HOME)/.local/bin/muxd
	install -m 0755 $(BIN_DIR)/muxctl $(HOME)/.local/bin/muxctl
	install -m 0755 $(BIN_DIR)/shenmux-web $(HOME)/.local/bin/shenmux-web

clean:
	rm -rf $(BIN_DIR) dist
