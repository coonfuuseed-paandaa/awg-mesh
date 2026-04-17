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

# v1.8.0 release gate targets.
# smoke-v18: fast (<2 min) binary + CLI checks, builds images from source.
# e2e-v18:   full mesh-up + FR-2/FR-3/FR-4 verification (<10 min).
# release-gate: both must pass before gh release create v1.8.0.
# Fails cleanly (exit 2) if Docker is not running — does NOT hang.

.PHONY: smoke-v18 e2e-v18 release-gate

smoke-v18:
	@command -v docker >/dev/null 2>&1 || { echo "ERROR: docker not found; install Docker 24+ first"; exit 2; }
	@docker info >/dev/null 2>&1 || { echo "ERROR: docker not running; start Docker and retry"; exit 2; }
	bash tests/v18_smoke/smoke.sh

e2e-v18:
	@command -v docker >/dev/null 2>&1 || { echo "ERROR: docker not found; install Docker 24+ first"; exit 2; }
	@docker info >/dev/null 2>&1 || { echo "ERROR: docker not running; start Docker and retry"; exit 2; }
	bash tests/v18_smoke/e2e.sh

release-gate: smoke-v18 e2e-v18
	@echo "release-gate: PASS — v1.8.0 is clear for tagging"
