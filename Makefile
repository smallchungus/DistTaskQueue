.PHONY: help test test-unit test-integration lint build run docker docker-up docker-down k8s-validate install-hooks

help:
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' Makefile | awk 'BEGIN{FS=":.*?## "}{printf "  %-20s %s\n", $$1, $$2}'

test: test-unit test-integration ## Run all tests

test-unit: ## Run unit tests with race detector
	go test -race -count=1 ./...

test-integration: ## Run integration tests (requires Docker)
	go test -race -count=1 -tags=integration ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

install-hooks: ## Install git pre-commit and pre-push hooks
	./scripts/install-hooks.sh

build: ## Build the api binary
	CGO_ENABLED=0 go build -o bin/api ./cmd/api

run: ## Run the api binary locally
	go run ./cmd/api

docker: ## Build the api Docker image
	docker build -t disttaskqueue/api:dev .

docker-up: ## Start local dev stack (api, postgres, redis)
	docker compose up --build

docker-down: ## Stop local dev stack
	docker compose down -v

k8s-validate: ## Validate k8s manifests offline (requires kubeconform)
	kubeconform -summary -strict deploy/k8s/
