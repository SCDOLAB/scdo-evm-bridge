BINARY  := parallel-node
DATADIR ?= $(CURDIR)/data
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w

.PHONY: all build run vet fmt fmt-check test clean install

all: build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) .

# Runs a local node with its own data directory (state.json, headers.dat, txs.jsonl, faucet.json).
run: build
	mkdir -p $(DATADIR)
	SCDO_DATADIR=$(DATADIR) ./$(BINARY)

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

test:
	go test ./...

clean:
	rm -f $(BINARY)

install: build
	install -m 0755 $(BINARY) /usr/local/bin/$(BINARY)
