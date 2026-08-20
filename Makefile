.PHONY: all build build-api build-ui test test-unit test-ui test-api run-api run-ui fmt fmt-check lint tidy clean docker-build-api docker-build-ui help

BIN_DIR ?= bin
GO ?= go

all: build test

## build: Build all canary binaries (api-canary and ui-canary)
build: build-api build-ui

## build-api: Build the API canary binary
build-api:
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/api-canary ./cmd/api-canary

## build-ui: Build the UI canary binary
build-ui:
	@mkdir -p $(BIN_DIR)
	$(GO) build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/ui-canary ./cmd/ui-canary

## run-api: Run the API canary locally
run-api:
	$(GO) run ./cmd/api-canary

## run-ui: Run the UI canary locally (reads remaining config from .env)
run-ui:
	CANARY_TARGET=$${CANARY_TARGET:-staging-us-central1} \
	CANARY_ENVIRONMENT=$${CANARY_ENVIRONMENT:-staging} \
	CANARY_REGION=$${CANARY_REGION:-us-central1} \
	CANARY_RUNTIME=$${CANARY_RUNTIME:-local} \
	CANARY_METRICS_EXPORTER=$${CANARY_METRICS_EXPORTER:-none} \
	CANARY_LOCK_BACKEND=$${CANARY_LOCK_BACKEND:-none} \
	$(GO) run ./cmd/ui-canary

## test: Run all tests in the repository
test:
	$(GO) test -v ./...

## test-unit: Run short unit tests
test-unit:
	$(GO) test -v -short ./...

## test-ui: Run UI canary tests
test-ui:
	$(GO) test -v ./internal/uicanary/...

## test-api: Run API canary tests
test-api:
	$(GO) test -v ./internal/canaryapi/... ./internal/lifecycle/...

## fmt: Format all Go source files
fmt:
	gofmt -s -w .

## fmt-check: Check formatting of all Go source files
fmt-check:
	@test -z "$$(gofmt -s -l .)" || (echo "Unformatted files found. Run 'make fmt':" && gofmt -s -l . && exit 1)

## lint: Run go vet on all packages
lint: fmt-check
	$(GO) vet ./...

## tidy: Download and tidy Go module dependencies
tidy:
	$(GO) mod tidy

## clean: Remove built binaries and test artifacts
clean:
	rm -rf $(BIN_DIR) /tmp/ui-canary-artifacts

## docker-build-api: Build Docker image for API canary
docker-build-api:
	docker build --target default -t superserve/api-canary:latest .

## docker-build-ui: Build Docker image for UI canary
docker-build-ui:
	docker build --target ui-canary -t superserve/ui-canary:latest .

## help: Display this help message
help:
	@echo "Usage: make [target]"
	@echo ""
	@echo "Targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed -e 's/## //g' | awk -F ':' '{printf "  %-18s %s\n", $$1, $$2}'
