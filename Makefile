BIN := bin/mycel
LDFLAGS := -X github.com/richard-sen27/mycelium/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PKG := github.com/richard-sen27/mycelium

.PHONY: all build test lint gen gen-check tools e2e clean

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/mycel

test:
	go test ./...

e2e:
	go test -tags e2e -count=1 -timeout 10m ./test/e2e/...

lint:
	golangci-lint run ./...
	buf lint

tools:
	go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
	go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.21.1

gen:
	buf generate

gen-check: gen
	git diff --exit-code -- gen/

clean:
	rm -rf bin dist
