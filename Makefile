.PHONY: build test lint clean

BINARY  := log-tools
GOOS    ?= $(shell go env GOOS)
GOARCH  ?= $(shell go env GOARCH)

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags="-s -w" -o $(BINARY) .

test:
	go test -v -race ./...

lint:
	go vet ./...

clean:
	rm -f $(BINARY)
