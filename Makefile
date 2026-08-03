SHELL := /bin/bash
BIN_DIR ?= bin

.PHONY: all build test race vet guards guard-check audit shen bifrost check clean install

all: check build

build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/muxd ./cmd/muxd
	go build -o $(BIN_DIR)/muxctl ./cmd/muxctl

test:
	go test ./...

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

check: guard-check audit test vet
	go build ./...
	CGO_ENABLED=0 go build ./...

install: build
	install -m 0755 $(BIN_DIR)/muxd $(HOME)/.local/bin/muxd
	install -m 0755 $(BIN_DIR)/muxctl $(HOME)/.local/bin/muxctl

clean:
	rm -rf $(BIN_DIR) dist
