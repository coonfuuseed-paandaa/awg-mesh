# Manual end-to-end test target for client ECMP (US1 failover + US2 stickiness).
# Requires: Linux host (or WSL2), Docker 24+, Docker Compose v2.
# Gracefully skips if Docker is not available so CI (which lacks Docker) is unaffected.
# Run manually: make test-client-ecmp

.DEFAULT_GOAL := build

GOFLAGS := -trimpath
LDFLAGS := -s -w
BIN_DIR := bin

.PHONY: build install install-all test lint proto-gen docker clean

install:
	go install $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/mesh-ctl

install-all:
	go install $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/mesh-ctl
	go install $(GOFLAGS) -ldflags "$(LDFLAGS)" ./cmd/awg-mesh-node

build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/awg-mesh-node ./cmd/awg-mesh-node
	CGO_ENABLED=0 GOOS=linux go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/mesh-ctl ./cmd/mesh-ctl

test:
	go test -race -cover ./...

lint:
	golangci-lint run ./...

proto-gen:
	protoc --proto_path=proto --go_out=proto --go_opt=paths=source_relative --go-grpc_out=proto --go-grpc_opt=paths=source_relative proto/*.proto

docker:
	docker build -f deploy/Dockerfile -t awg-mesh-node:$(VERSION) .

clean:
	rm -rf $(BIN_DIR)

.PHONY: test-client-ecmp
test-client-ecmp:
	@command -v docker >/dev/null 2>&1 || { echo "Docker not available; skipping client-ecmp e2e test"; exit 0; }
	@docker info >/dev/null 2>&1 || { echo "Docker daemon not running; skipping"; exit 0; }
	bash tests/client_ecmp/verify.sh
