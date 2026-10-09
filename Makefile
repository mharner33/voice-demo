SHELL := /bin/bash
GO    ?= go
BIN   := bin
PKG   := github.com/mharner33/voice-demo

# podman-compose if present, else `podman compose`
COMPOSE := $(shell command -v podman-compose >/dev/null 2>&1 && echo podman-compose || echo "podman compose")
COMPOSE_FILE := deploy/compose.yml

# Where the demo targets expect to find a running gateway.
DEMO_CONTROL ?= 127.0.0.1:50051
DEMO_HTTP    ?= 127.0.0.1:8080
SIP_LOG      ?= /tmp/sip_signaling.log

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

## dashboard: upload deploy/datadog/dashboard.json (needs DD_API_KEY and DD_APP_KEY)
dashboard:
	@test -n "$$DD_API_KEY" || { echo "DD_API_KEY is not set"; exit 1; }
	@test -n "$$DD_APP_KEY" || { echo "DD_APP_KEY is not set"; exit 1; }
	curl -sS -X POST "https://api.$${DD_SITE:-datadoghq.com}/api/v1/dashboard" \
	  -H "Content-Type: application/json" \
	  -H "DD-API-KEY: $$DD_API_KEY" \
	  -H "DD-APPLICATION-KEY: $$DD_APP_KEY" \
	  -d @deploy/datadog/dashboard.json | python3 -m json.tool | head -20

## demo: scripted demo run — four beats against a gateway you already started
demo:
	CONTROL=$(DEMO_CONTROL) HTTP=$(DEMO_HTTP) deploy/demo.sh

## load: place 50 calls against a running gateway
load:
	./bin/voicectl load -control $(DEMO_CONTROL) -calls 50 -concurrency 10 -profile clean,mobile,lossy-wan

## fixtures: write the built-in synthetic call audio as WAV files
fixtures:
	./bin/voicectl fixtures -dir testdata/fixtures

## sip-logs: reconstruct SIP signaling from the gateway's call log
sip-logs:
	@test -n "$(CALL_LOG)" || { echo "set CALL_LOG=<the gateway's -call-log path>"; exit 1; }
	python3 deploy/sip/sip_log_generator.py --call-log $(CALL_LOG) --out $(SIP_LOG) --follow

## monitors: upload deploy/datadog/monitors/*.json (needs DD_API_KEY and DD_APP_KEY)
monitors:
	@test -n "$$DD_API_KEY" || { echo "DD_API_KEY is not set"; exit 1; }
	@test -n "$$DD_APP_KEY" || { echo "DD_APP_KEY is not set"; exit 1; }
	@for f in deploy/datadog/monitors/*.json; do \
	  echo "uploading $$f"; \
	  curl -sS -X POST "https://api.$${DD_SITE:-datadoghq.com}/api/v1/monitor" \
	    -H "Content-Type: application/json" \
	    -H "DD-API-KEY: $$DD_API_KEY" \
	    -H "DD-APPLICATION-KEY: $$DD_APP_KEY" \
	    -d @$$f | python3 -c 'import json,sys; d=json.load(sys.stdin); print("  ->", d.get("id", d))'; \
	done

.PHONY: help build test test-integration cover fuzz lint tidy proto up down logs \
	agent-status dashboard monitors demo load fixtures sip-logs
