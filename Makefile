.PHONY: all build test lint clean tidy fmt-check install docker e2e

GO_ENV := GOWORK=off GOFLAGS=-mod=mod

LDFLAGS := -s -w \
	-X 'github.com/avkcode/spinnaker-cli/cmd.Version=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)' \
	-X 'github.com/avkcode/spinnaker-cli/cmd.Commit=$(shell git rev-parse --short HEAD 2>/dev/null || echo none)' \
	-X 'github.com/avkcode/spinnaker-cli/cmd.BuildDate=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)'

all: build

build:
	$(GO_ENV) go build -ldflags="$(LDFLAGS)" -o sc .

install: build
	install -m 0755 sc $(or $(PREFIX),/usr/local)/bin/sc

test:
	$(GO_ENV) go test ./...

# End-to-end suite against a real Spinnaker. Requires SC_E2E_GATE (and
# optionally SC_E2E_USER / SC_E2E_PASSWORD / SC_E2E_KUBECONFIG); skips otherwise.
e2e: build
	$(GO_ENV) SC_E2E=1 go test -count=1 -v -timeout 20m ./test/e2e/...

lint-install:
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "Installing golangci-lint..."; \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(shell go env GOPATH)/bin v2.12.2; \
	}

lint: lint-install
	$(shell go env GOPATH)/bin/golangci-lint run ./...

tidy:
	$(GO_ENV) go mod tidy

clean:
	go clean
	rm -f sc

fmt-check:
	@files=$$(find . -path './vendor/*' -prune -o -name '*.go' -print); \
	unformatted=$$(gofmt -l $$files); \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' "$$unformatted"; \
		exit 1; \
	fi

docker:
	docker build -t avkcode/spinnaker-cli:latest .
