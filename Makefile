# pg_query_go uses cgo; silence macOS deployment-target linker warnings.
export MACOSX_DEPLOYMENT_TARGET ?= 13.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test test-short lint proto pg clean

build:
	go build -ldflags "$(LDFLAGS)" -o bin/changegate ./cmd/changegate
	go build -o bin/changegate-adapter-toykv ./adapters/toykv/cmd/changegate-adapter-toykv

test:
	go test ./...

test-short:
	go test -short ./...

lint:
	golangci-lint run ./...

proto:
	protoc -I proto --go_out=gen --go_opt=paths=source_relative \
	  --go-grpc_out=gen --go-grpc_opt=paths=source_relative proto/changegate/v1/*.proto

pg:
	./scripts/test-pg.sh

clean:
	rm -rf bin dist
