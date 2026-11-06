# Nexus Agent Hub — build & test entrypoints

BINARY_NEXUS := bin/nexus
BINARY_ECHOD := bin/echod

.PHONY: all build echod test vet fmt check clean

all: build

build: $(BINARY_NEXUS) $(BINARY_ECHOD)

$(BINARY_NEXUS):
	go build -o $(BINARY_NEXUS) ./cmd/nexus

$(BINARY_ECHOD):
	go build -o $(BINARY_ECHOD) ./cmd/echod

test:
	go test ./...

test-e2e:
	go test -tags=e2e ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# CI gate: gofmt -l must be empty
check: fmt vet test
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed on: $$out"; exit 1; fi

clean:
	rm -rf bin/ data/
