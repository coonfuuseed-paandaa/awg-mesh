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
