SHELL := /bin/bash

default: help
.PHONY: default

help: ## Display this help screen (default)
	@grep -h "##" $(MAKEFILE_LIST) | grep -vE '^#|grep' | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}' | sort
.PHONY: help

all: clean build test test-bench test-stress test-integration ## Run all steps: clean, build, test, test-bench, test-stress, test-integration
	@echo "Test Failures: "
	@grep -ci 'FAIL:' /tmp/caddy-consul-* || true
.PHONY: all

lint: ## Run linter against codebase
	@golangci-lint -v run
.PHONY: lint

# GO_VERSION should (generally) match go.mod version (CADDY_VERSION must also match in go.mod)
build: export GO_VERSION       ?= 1.25.6
build: export XCADDY_VERSION   ?= 0.4.5
build: export CADDY_VERSION    ?= 2.11.2
build: export CADDY_L4_VERSION ?= afd229714fb14a387f0736cab048afeb72b8946a
build: lint ## Run 'docker composer build' to build caddy with plugin, copy output binary to ./bin/caddy
	@docker compose build --build-arg GO_VERSION=$(GO_VERSION) --build-arg XCADDY_VERSION=$(XCADDY_VERSION) --build-arg CADDY_VERSION=$(CADDY_VERSION) --build-arg CADDY_L4_VERSION=$(CADDY_L4_VERSION)
	@CID=$$(docker create caddy-consul-integration-test:latest);          \
		docker cp $$CID:/usr/local/bin/caddy ./bin/caddy >/dev/null 2>&1;   \
		docker rm $$CID >/dev/null
.PHONY: build

test: export TEST       ?= Test[^I]
test: export TEST_DIR   ?= ./...
test: export TEST_COUNT ?= 1
test: test-setup ## Run basic unit tests: TEST=.* TEST_DIR=./... TEST_COUNT=1 make test
	@go test -v -race -count=$(TEST_COUNT) -run "$(TEST)" $(TEST_DIR) 2>&1 | tee /tmp/caddy-consul-test.log
	@echo "Test completed, see /tmp/caddy-consul-test.log for details"
.PHONY: test

test-bench: export TEST       ?= ^$
test-bench: export TEST_BENCH ?= .
test-bench: export TEST_DIR   ?= ./...
test-bench: test-setup ## Run bench tests: TEST_BENCH="." TEST_DIR=./... make test-benchm
	@go test -run="$(TEST)" -bench="$(TEST_BENCH)" -benchmem -benchtime=10s -timeout=5m $(TEST_DIR) 2>&1 | tee /tmp/caddy-consul-bench.log
	@echo "BenchTest completed, see /tmp/caddy-consul-bench.log for details"
.PHONY: test-bench

test-stress: export TEST       ?= TestConcurrent
test-stress: export TEST_DIR   ?= ./...
test-stress: export TEST_COUNT ?= 100
test-stress: test-setup ## Run stress tests: TEST=TestConcurrent TEST_DIR=./... TEST_COUNT=100 make test-stress
	@go test -v -race -count=$(TEST_COUNT) -run "$(TEST)" $(TEST_DIR) 2>&1 | tee /tmp/caddy-consul-stress.log
	@echo "StressTest completed, see /tmp/caddy-consul-stress.log for details"
.PHONY: test-stress

test-setup:
	@go clean -testcache
.PHONY: test-setup

test-integration: export TEST     ?= TestIntegration
test-integration: export TEST_DIR ?= ./integration-test/...
test-integration: test-setup test-integration-setup ## Run integration tests with Docker Compose
	@echo "Running integration tests..."
	@go test -v -timeout=300s -run "$(TEST)" $(TEST_DIR) 2>&1 | tee /tmp/caddy-consul-integration.log
	@echo "IntegrationTest completed, see /tmp/caddy-consul-integration.log for details"
.PHONY: test-integration

test-integration-setup:
	@docker compose up -d --wait
.PHONY: test-integration-setup

test-integration-cleanup:
	@docker compose down || true
	@rm -rf integration-test/cache/* 2>/dev/null || true
.PHONY: test-integration-cleanup

fmt: ## Run go-fmt against codebase
	@go fmt ./...
.PHONY: fmt

mod-download: ## Download go modules
	@go mod download
.PHONY: mod-download

mod-tidy: ## Make sure go modules are tidy
	@go mod tidy
.PHONY: mod-tidy

mod-update:
	@if [[ -n "${MODULE}" ]] && [[ -n "${MODULE_VERSION}" ]]; then          \
		echo "Running 'go list -m ${MODULE}@${MODULE_VERSION}' ...";        \
		GOPROXY=proxy.golang.org go list -m "${MODULE}@${MODULE_VERSION}";  \
	else                                                                    \
		echo "ERROR: Missing 'MODULE'/'MODULE_VERSION', cannot continue";   \
		exit 1;                                                             \
	fi
.PHONY: mod-update

clean: test-integration-cleanup ## Clean up repo
	@docker rmi -f caddy-consul-integration-test:latest 2>/dev/null || true
	@rm -f bin/caddy 2>/dev/null || true
.PHONY: clean

release: export MODULE         ?= github.com/honest-hosting/caddy-consul
release: export MODULE_VERSION ?=
release: mod-update ## Run release step(s) for module version: MODULE_VERSION=v0.0.1 make release
.PHONY: release
