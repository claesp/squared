# Common tasks. Run "make help" to see them.

VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
GO      ?= go
TEST_DB_CONTAINER := squared-test-db
TEST_DB_PORT      := 15433
TEST_DATABASE_URL := postgres://squared:test@127.0.0.1:$(TEST_DB_PORT)/squared?sslmode=disable

.PHONY: help
help: ## Show this help.
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*## "} {printf "  %-18s %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the squared binary into bin/.
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/squared ./cmd/squared

.PHONY: check
check: ## Check formatting, run go vet and check the catalog.
	@test -z "$$(gofmt -l .)" || { echo "gofmt finds problems in:"; gofmt -l .; exit 1; }
	$(GO) vet ./...
	$(GO) run ./cmd/squared check-catalog catalog >/dev/null

.PHONY: test
test: ## Run the unit tests. Tests that need PostgreSQL are skipped.
	$(GO) test -race ./...

.PHONY: test-db
test-db: ## Start a PostgreSQL container for the integration tests.
	@podman container exists $(TEST_DB_CONTAINER) || podman run -d --name $(TEST_DB_CONTAINER) \
		-e POSTGRES_USER=squared -e POSTGRES_PASSWORD=test -e POSTGRES_DB=squared \
		-p 127.0.0.1:$(TEST_DB_PORT):5432 docker.io/library/postgres:17-alpine >/dev/null
	@podman start $(TEST_DB_CONTAINER) >/dev/null
	@until podman exec $(TEST_DB_CONTAINER) pg_isready -U squared >/dev/null 2>&1; do sleep 1; done

.PHONY: test-all
test-all: test-db ## Run all tests, also the tests that need PostgreSQL.
	SQUARED_TEST_DATABASE_URL="$(TEST_DATABASE_URL)" $(GO) test -race ./...

.PHONY: image
image: ## Build the container image localhost/squared:dev.
	podman build --tag localhost/squared:dev --build-arg VERSION=$(VERSION) .

.PHONY: kind-up
kind-up: ## Make the complete development environment in Kind.
	hack/kind-up.sh

.PHONY: kind-deploy
kind-deploy: ## Build and deploy Squared to the Kind cluster again.
	hack/kind-deploy.sh

.PHONY: kind-down
kind-down: ## Remove the Kind cluster.
	hack/kind-down.sh

.PHONY: e2e
e2e: ## Run the end-to-end test against the Kind cluster.
	hack/e2e.sh
