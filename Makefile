GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test e2e vet clean

# The binary is mounted into machines, so it must be static.
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/cloudsourced ./cmd/cloudsourced

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

e2e: build
	./test/e2e/run.sh

clean:
	rm -rf bin
