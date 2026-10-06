GO ?= go
SQLC ?= $(GO) run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
GOLANGCI_LINT ?= $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
BIN_DIR ?= bin
HELPER_ARCH ?= $(shell docker info --format '{{.Architecture}}' 2>/dev/null | sed 's/aarch64/arm64/;s/x86_64/amd64/')
VERSION ?= dev
EXE_SUFFIX = $(if $(filter windows,$(shell $(GO) env GOOS)),.exe,)
GO_FORMAT_PATHS = cmd internal examples/starter assets.go schemas/embed.go

.PHONY: build help helper firewall test test-race vet format format-check check lint integration clean generate-sql check-sql

build:
	$(GO) build -mod=readonly -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/knotra$(EXE_SUFFIX) ./cmd/knotra

help:
	@printf '%s\n' \
		'make build         Build the CLI into $(BIN_DIR)' \
		'make test          Run unit tests' \
		'make test-race     Run tests with the race detector' \
		'make lint          Run strict golangci-lint checks' \
		'make vet           Run go vet' \
		'make format        Format Go sources' \
		'make format-check  Check Go formatting' \
		'make check         Run lint, formatting, vet and race tests' \
		'make integration   Run opt-in integration tests (set KNOTRA_TEST_* variables)' \
		'make generate-sql  Regenerate sqlc queries' \
		'make check-sql     Check that generated queries are committed' \
		'make helper        Build the Linux sandbox helper' \
		'make firewall      Build the sandbox firewall image' \
		'make clean         Remove CLI build output'

generate-sql:
	$(SQLC) generate

check-sql: generate-sql
	git diff --exit-code -- internal/store/db

helper:
	@test -n "$(HELPER_ARCH)" || (echo 'Docker must be running, or set HELPER_ARCH=amd64/arm64'; exit 1)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(HELPER_ARCH) $(GO) build -mod=readonly -trimpath -o .knotra/bin/sandbox-helper ./internal/adapters/sandboxhelper

firewall:
	docker build -t knotra-firewall:dev internal/adapters/firewall

test:
	$(GO) test -mod=readonly ./...

test-race:
	$(GO) test -mod=readonly -race ./...

vet:
	$(GO) vet -mod=readonly ./...

format:
	gofmt -w $(GO_FORMAT_PATHS)

format-check:
	@test -z "$$(gofmt -l $(GO_FORMAT_PATHS))" || (echo 'Run make format'; exit 1)

lint:
	$(GOLANGCI_LINT) config verify
	$(GOLANGCI_LINT) run

check: lint format-check vet test-race

integration:
	$(GO) test -mod=readonly -count=1 -timeout=20m ./internal/store ./internal/api ./internal/executor ./internal/queue ./internal/scheduler ./internal/adapters/... ./internal/integration/...

clean:
	rm -rf $(BIN_DIR)
