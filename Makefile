.PHONY: build test vet clean

# Version comes from git tags. Falls back to the commit hash, then "dev".
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o snsync ./cmd/snsync

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f snsync
