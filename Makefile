# go-agent-worker — developer tasks
# Note: run `go mod tidy` after the first `go get` (see docs/DECISIONS.md).

BINARY := bin/agent
PKG    := ./cmd/agent

.PHONY: build run tidy fmt vet test clean

build: ## Compile the agent binary into ./bin
	go build -o $(BINARY) $(PKG)

run: ## Run the agent (reads .env via your shell / a loader)
	go run $(PKG)

tidy: ## Sync go.mod/go.sum
	go mod tidy

fmt: ## Format all Go source
	go fmt ./...

vet: ## Static checks
	go vet ./...

test: ## Run tests
	go test ./...

clean: ## Remove build output
	rm -rf bin dist
