.PHONY: help run build test test-race cover fmt vet lint clean tidy

ADMIN_API_TOKEN ?= dev-token
DB_PATH ?= ./data/qdt.db

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

run: ## Run the server (migrations apply on boot)
	ADMIN_API_TOKEN=$(ADMIN_API_TOKEN) DB_PATH=$(DB_PATH) go run ./cmd/server

build: ## Build a static binary (no cgo, so it cross-compiles anywhere)
	CGO_ENABLED=0 go build -o server ./cmd/server

test: ## Run the test suite
	go test ./... -count=1

test-race: ## Run under the race detector (needs cgo and a C compiler; see README)
	CGO_ENABLED=1 go test ./... -race -count=1

cover: ## Report total test coverage
	go test ./... -count=1 -coverpkg=./internal/...,./migrations/... -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

fmt: ## Format all Go source
	gofmt -w .

vet: ## Run go vet
	go vet ./...

lint: fmt vet ## Format and vet

tidy: ## Tidy module dependencies
	go mod tidy

clean: ## Remove build output and local databases
	rm -f server coverage.out
	rm -rf data
