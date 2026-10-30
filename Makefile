GO ?= go
BIN := bin/mrcpgo

.PHONY: all build test race integration vet fmt fmt-check clean run

all: build

build:
	$(GO) build -o $(BIN) ./cmd/mrcpgo

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

integration:
	$(GO) test -tags=integration ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "unformatted files:"; echo "$$out"; exit 1; fi

run:
	$(GO) run ./cmd/mrcpgo -config config.example.yaml

clean:
	rm -rf bin