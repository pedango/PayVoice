BINARY  := payvoice
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: help build run test race cover vet fmt lint tidy docker clean

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/payvoice

run: ## Run with the tone TTS provider, which needs no credentials
	PAYVOICE_TTS_PROVIDER=tone PAYVOICE_LOG_LEVEL=debug go run ./cmd/payvoice

test: ## Run the tests
	go test ./... -count=1

race: ## Run the tests under the race detector (needs cgo)
	CGO_ENABLED=1 go test -race ./... -count=1

cover: ## Write and open a coverage report
	go test ./... -coverprofile=coverage.out -covermode=atomic
	go tool cover -html=coverage.out

vet: ## Run go vet
	go vet ./...

fmt: ## Format the tree
	gofmt -w .

lint: fmt vet ## Format, then vet

tidy: ## Tidy and verify modules
	go mod tidy
	go mod verify

docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t payvoice:$(VERSION) -t payvoice:latest .

clean: ## Remove build artefacts
	rm -f $(BINARY) $(BINARY).exe coverage.out
