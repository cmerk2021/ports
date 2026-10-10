# Simple build/test helpers for ports.
PREFIX  ?= /usr/local
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/cmerk2021/ports/internal/cli.Version=$(VERSION)

.PHONY: all build test test-short vet fmt install clean

all: vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ports ./cmd/ports

test:
	go test -race ./...

test-short:
	go test -short ./...

vet:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

fmt:
	gofmt -w .

install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 0755 bin/ports $(DESTDIR)$(PREFIX)/bin/ports

clean:
	rm -rf bin
