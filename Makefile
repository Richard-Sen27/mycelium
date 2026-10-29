BIN := bin/mycel
LDFLAGS := -X github.com/richard-sen27/mycelium/internal/version.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PKG := github.com/richard-sen27/mycelium

.PHONY: all build test lint gen gen-check gen-web web web-check tools e2e e2e-ui clean

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/mycel

test:
	go test ./...

e2e:
	go test -tags e2e -count=1 -timeout 10m ./test/e2e/...

# e2e-ui builds the UI first, so the e2e binary embeds it
# (TestUIServedAndEvents checks the app then; the browser smoke test with
# MYCEL_TEST_BROWSER=1 needs it). CI builds the UI before its e2e step.
e2e-ui: web e2e

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

# UI: web/ is a pnpm project (Node 22). `make web` builds it into
# internal/web/dist, which the binary embeds; without it the binary serves
# a "UI not built" page. `make gen-web` regenerates the TypeScript client
# from proto/ (needs `pnpm install` in web/ first).
web:
	cd web && pnpm install --frozen-lockfile && pnpm build

web-check: web
	cd web && pnpm test && pnpm run size-check

gen-web:
	buf generate --template buf.gen.web.yaml

clean:
	rm -rf bin dist web/dist
	find internal/web/dist -mindepth 1 ! -name .keep -delete
