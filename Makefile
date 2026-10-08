SHELL := /bin/bash
GO    ?= go
BIN   := bin
PKG   := github.com/mharner33/voice-demo

# podman-compose if present, else `podman compose`
COMPOSE := $(shell command -v podman-compose >/dev/null 2>&1 && echo podman-compose || echo "podman compose")
COMPOSE_FILE := deploy/compose.yml

.DEFAULT_GOAL := help

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## build: build both binaries into ./bin
build:
	@mkdir -p $(BIN)
	$(GO) build -o $(BIN)/voicegw  ./cmd/voicegw
	$(GO) build -o $(BIN)/voicectl ./cmd/voicectl
	@echo "built: $(BIN)/voicegw $(BIN)/voicectl"

## test: unit + offline integration tests (no cloud, no credentials)
test:
	$(GO) test -race -count=1 ./...

## test-integration: tests that hit real cloud providers (costs money)
test-integration:
	$(GO) test -race -count=1 -tags=integration ./...

## cover: test with coverage report
cover:
	$(GO) test -race -count=1 -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -20

## fuzz: short fuzz run over the G.711 decoder
fuzz:
	$(GO) test ./internal/codec -run=NONE -fuzz=FuzzULawDecode -fuzztime=30s

## lint: vet + gofmt check
lint:
	$(GO) vet ./...
	@out=$$(gofmt -l . ); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	@echo "lint ok"

## tidy: go mod tidy
tidy:
	$(GO) mod tidy

## proto: regenerate gRPC control plane from proto/
proto:
	protoc --go_out=. --go_opt=module=$(PKG) \
	       --go-grpc_out=. --go-grpc_opt=module=$(PKG) \
	       proto/*.proto
	@echo "proto generated"

## up: start voicegw + datadog-agent under podman
up:
	$(COMPOSE) -f $(COMPOSE_FILE) up -d --build
	@echo "waiting for agent..."; sleep 5
	@$(MAKE) --no-print-directory agent-status

## down: stop the stack
down:
	$(COMPOSE) -f $(COMPOSE_FILE) down -v

## logs: tail stack logs
logs:
	$(COMPOSE) -f $(COMPOSE_FILE) logs -f

## agent-status: verify APM + DogStatsD are ready inside the agent
agent-status:
	@podman exec datadog-agent agent status 2>/dev/null \
	  | grep -A4 -E 'APM Agent|DogStatsD' || echo "agent not ready"

## demo: scripted demo run (phase 8)
demo:
	@echo "not implemented until phase 8"

.PHONY: help build test test-integration cover fuzz lint tidy proto up down logs agent-status demo
