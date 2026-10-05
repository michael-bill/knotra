GO ?= go
BIN_DIR ?= bin
HELPER_ARCH ?= $(shell docker info --format '{{.Architecture}}' 2>/dev/null | sed 's/aarch64/arm64/;s/x86_64/amd64/')
VERSION ?= dev
EXE_SUFFIX = $(if $(filter windows,$(shell $(GO) env GOOS)),.exe,)


.PHONY: build helper firewall test check integration clean

build:
	$(GO) build -mod=readonly -trimpath -ldflags "-X main.version=$(VERSION)" -o $(BIN_DIR)/knotra$(EXE_SUFFIX) ./cmd/knotra

helper:
	@test -n "$(HELPER_ARCH)" || (echo 'Docker must be running, or set HELPER_ARCH=amd64/arm64'; exit 1)
	CGO_ENABLED=0 GOOS=linux GOARCH=$(HELPER_ARCH) $(GO) build -mod=readonly -trimpath -o .knotra/bin/sandbox-helper ./internal/adapters/sandboxhelper

firewall:
	docker build -t knotra-firewall:dev internal/adapters/firewall

test:
	$(GO) test -mod=readonly ./...

check:
	@test -z "$$(gofmt -l cmd internal examples/starter assets.go schemas/embed.go)" || (echo 'Run gofmt on Go sources'; exit 1)
	$(GO) vet -mod=readonly ./...
	$(GO) test -mod=readonly -race ./...

integration:
	$(GO) test -mod=readonly -count=1 -timeout=20m ./internal/store ./internal/api ./internal/adapters/... ./internal/integration/...

clean:
	rm -rf $(BIN_DIR)
