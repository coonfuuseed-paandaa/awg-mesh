.DEFAULT_GOAL := build

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION) -s -w
GOFLAGS := -trimpath
BIN_DIR := bin

.PHONY: build test lint proto-gen docker clean

build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 GOOS=linux go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/awg-mesh-node ./cmd/awg-mesh-node
	CGO_ENABLED=0 GOOS=linux go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/mesh-ctl ./cmd/mesh-ctl

test:
	go test -race -cover ./...

lint:
	golangci-lint run ./...

proto-gen:
	protoc --proto_path=proto --go_out=. --go-grpc_out=. proto/*.proto

docker:
	docker build -f deploy/Dockerfile -t awg-mesh-node:$(VERSION) .

clean:
	rm -rf $(BIN_DIR)
